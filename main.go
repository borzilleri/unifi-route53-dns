package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/gregdel/pushover"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const DEFAULT_PORT int = 8080
const DEFAULT_LOG_LEVEL zerolog.Level = zerolog.DebugLevel

const CONFIG_FILE_PATH string = "config.yaml"
const ENV_PREFIX string = "UNIFI_R53_DNS_"
const ENV_LOG_LEVEL string = ENV_PREFIX + "LOG_LEVEL"
const ENV_DEV_MODE = "DEV_MODE"

type Hostname string
type ZoneID string
type TTL int64

type HealthResponse struct {
	Status string `json:"status"`
}

type HostConfig struct {
	ZoneId          string   `koanf:"zoneId"`
	Ttl             int64    `koanf:"ttl"`
	AdditionalHosts []string `koanf:"additionalHosts"`
}

type PushoverConfig struct {
	ApiToken     string `koanf:"api-token"`
	RecipientKey string `koanf:"user-key"`
}

type AppConfig struct {
	App struct {
		Port int `koanf:"port"`
	} `koanf:"app"`
	Records  map[string]HostConfig `koanf:"records"`
	Pushover PushoverConfig        `koanf:"pushover"`
}

type UpdateRequest struct {
	Host   string
	IP     string
	Commit bool
}

func (r *UpdateRequest) getHostname() Hostname {
	return Hostname(r.Host)
}

func (r *UpdateRequest) validateIp() (net.IP, error) {
	ip := net.ParseIP(r.IP).To4()
	if ip == nil {
		return nil, fmt.Errorf("'%s' is not a valid IpV4 address", r.IP)
	}
	return ip, nil
}

func NotifyPushover(config PushoverConfig, host string, err error) {
	if config.ApiToken == "" || config.RecipientKey == "" {
		log.Info().Msg("Pushover notification not configured.")
		return
	}
	app := pushover.New(config.ApiToken)
	recipient := pushover.NewRecipient(config.RecipientKey)
	msgStr := "IP Updated for: " + host
	if err != nil {
		msgStr = "Error: IP Update Failed for: " + host
	}
	message := &pushover.Message{
		Message:  msgStr,
		Priority: pushover.PriorityNormal,
	}

	// Send the message to the recipient
	response, err := app.SendMessage(message, recipient)
	if err != nil {
		log.Error().Err(err).
			Msg("Error publishing Pushover Notification")
	}
	log.Info().
		Interface("PushoverResponse", response).
		Msg("Pushover Response")
}

func MakeChangeRequest(zoneId ZoneID, hostname Hostname, ip net.IP, ttl TTL) route53.ChangeResourceRecordSetsInput {
	resourceHostName := string(hostname) + "."
	resourceTtl := int64(ttl)
	resourceZoneId := string(zoneId)
	resourceIp := ip.String()
	comment := "Unifi Updated IP Address"
	lastUpdatedMsg := fmt.Sprintf("\"Last Updated: %s\"", time.Now().String())
	return route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: &resourceHostName,
						Type: types.RRTypeA,
						TTL:  &resourceTtl,
						ResourceRecords: []types.ResourceRecord{
							{Value: &resourceIp},
						},
					},
				},
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: &resourceHostName,
						Type: types.RRTypeTxt,
						TTL:  &resourceTtl,
						ResourceRecords: []types.ResourceRecord{
							{Value: &lastUpdatedMsg},
						},
					},
				},
			},
			Comment: &comment,
		},
		HostedZoneId: &resourceZoneId,
	}
}

func UpdateRoute53Record(client route53.Client, hostConfig HostConfig, hostname Hostname, ip net.IP, commit bool) error {
	zoneId := ZoneID(hostConfig.ZoneId)
	ttl := TTL(hostConfig.Ttl)
	input := MakeChangeRequest(zoneId, hostname, ip, ttl)
	log.Debug().Interface("ChangeResourcesRecordSetInput", input).Send()
	if commit {
		output, err := client.ChangeResourceRecordSets(context.TODO(), &input)
		if err != nil {
			log.Error().Err(err).
				Interface("ChangeResourcesRecordSetOutput", output).
				Interface("ChangeResourcesRecordSetInput", input).
				Msg("Error Updating Route53 RecordSets")
			return err
		} else {
			log.Info().
				Str("hostname", string(hostname)).
				Interface("ChangeResourcesRecordSetOutput", output).
				Msg("RecordSet updated successfully.")
		}
	}
	return nil
}

func ProcessIpChange(client route53.Client, appConfig AppConfig, request UpdateRequest) {
	ip, err := request.validateIp()
	if err != nil {
		log.Error().Interface("request", request).Err(err).Msg("Invalid IP specified")
		return
	}

	if hostConfig, exists := appConfig.Records[request.Host]; exists {
		err := UpdateRoute53Record(client, hostConfig, request.getHostname(), ip, request.Commit)
		NotifyPushover(appConfig.Pushover, request.Host, err)
		if err != nil {
			// Process AWS request for this host, and additional hosts.
			for _, additionalHost := range hostConfig.AdditionalHosts {
				hostname := Hostname(additionalHost)
				err := UpdateRoute53Record(client, hostConfig, hostname, ip, request.Commit)
				if err != nil {
					NotifyPushover(appConfig.Pushover, string(hostname), err)
				}
			}
		}
	} else {
		log.Error().Interface("request", request).Msg("Hostname not found in config, ignoring")
	}
}

func HealthCheck(w http.ResponseWriter, _ *http.Request) {
	jsonStatus, _ := json.Marshal(HealthResponse{Status: "ok"})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonStatus)
}

func EnvVarTransformer(k, v string) (string, any) {
	k = strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(k, ENV_PREFIX)), "_", ".")
	return k, v
}

var (
	_, DevMode = os.LookupEnv(ENV_DEV_MODE)
	k          = koanf.New(".")
	parser     = yaml.Parser()
)

func LoadAppConfig() AppConfig {
	// Load our Config File
	if err := k.Load(file.Provider(CONFIG_FILE_PATH), parser); err != nil {
		log.Fatal().Err(err).Msg("Unable to load config.yaml")
	}

	// Load Environment Overrides
	k.Load(env.Provider(".", env.Opt{Prefix: ENV_PREFIX, TransformFunc: EnvVarTransformer}), nil)

	// Unmarshall the config into our struct
	var appConfig AppConfig
	k.Unmarshal("", &appConfig)

	if appConfig.App.Port <= 0 {
		log.Warn().Msg("app.port configured for non-positive value, using default value.")
		appConfig.App.Port = DEFAULT_PORT
	}

	log.Debug().Interface("config", appConfig).Msg("Loaded App Config")
	return appConfig
}

func LoadAwsConfig() aws.Config {
	config, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize AWS Config")
	}
	log.Debug().Msg("AWS Config Loaded")
	return config
}

func InitRoute53Client(config aws.Config) route53.Client {
	r53Client := route53.NewFromConfig(config)
	log.Debug().Msg("route53 Client Initialized")
	return *r53Client
}

func GetLogLevel() zerolog.Level {
	log_level := DEFAULT_LOG_LEVEL
	env_level, value_present := os.LookupEnv(ENV_LOG_LEVEL)
	if value_present {
		log_level, _ = zerolog.ParseLevel(env_level)
	}
	return log_level
}

func main() {
	// Check to switch to dev mode logging
	if DevMode {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	// Configure Log Level
	zerolog.SetGlobalLevel(GetLogLevel())

	appConfig := LoadAppConfig()
	awsConfig := LoadAwsConfig()
	r53Client := InitRoute53Client(awsConfig)

	nicUpdateHandler := func(w http.ResponseWriter, req *http.Request) {
		req.ParseForm()
		commitChanges := true
		if _, present := req.Form["commit"]; present {
			commitChanges, _ = strconv.ParseBool(req.FormValue("commit"))
		}
		updateRequest := UpdateRequest{
			Host:   req.FormValue("hostname"),
			IP:     req.FormValue("ip"),
			Commit: commitChanges,
		}
		log.Info().Interface("request", &updateRequest).Msg("Received Update Request")
		ProcessIpChange(r53Client, appConfig, updateRequest)
	}

	http.HandleFunc("/nic/update", nicUpdateHandler)
	http.HandleFunc("/health-check", HealthCheck)

	log.Info().Msg("Application Initialized")
	http.ListenAndServe(fmt.Sprintf(":%d", appConfig.App.Port), nil)
}
