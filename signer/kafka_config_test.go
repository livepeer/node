package signer

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func kafkaBrokerURL(t *testing.T, address string) KafkaBroker {
	t.Helper()
	var broker KafkaBroker
	require.NoError(t, broker.UnmarshalText([]byte(address)))
	return broker
}

func TestKafkaConfigurationValidation(t *testing.T) {
	require.ErrorContains(t, (KafkaConfig{}).Validate(), "broker")
	valid := KafkaConfig{Broker: kafkaBrokerURL(t, "kafka://broker:9092"), Topic: "signing", OutboxDB: "events.sqlite", OutboxMaxBytes: 1 << 20}
	require.NoError(t, valid.Validate())
	for _, mutate := range []func(*KafkaConfig){
		func(c *KafkaConfig) { c.Broker = KafkaBroker{} }, func(c *KafkaConfig) { c.Broker = kafkaBrokerURL(t, "kafka://one,two:9092") },
		func(c *KafkaConfig) { c.Broker = kafkaBrokerURL(t, "http://broker:9092") }, func(c *KafkaConfig) { c.Broker = kafkaBrokerURL(t, "kafka://broker:0") },
		func(c *KafkaConfig) { c.Broker = kafkaBrokerURL(t, "kafka://broker:99999") }, func(c *KafkaConfig) { c.Topic = "" },
		func(c *KafkaConfig) { c.Topic = "bad topic" }, func(c *KafkaConfig) { c.OutboxDB = "" },
		func(c *KafkaConfig) { c.OutboxDB = ":memory:" }, func(c *KafkaConfig) { c.OutboxMaxBytes = 0 },
		func(c *KafkaConfig) { c.Username = "username" }, func(c *KafkaConfig) { c.Password = "password" },
		func(c *KafkaConfig) { c.AuthMethod = "invalid" },
	} {
		c := valid
		mutate(&c)
		require.Error(t, c.Validate())
	}
	plainTransport, err := valid.transport()
	require.NoError(t, err)
	require.Nil(t, plainTransport.TLS)
	require.Nil(t, plainTransport.SASL)
	valid.Username, valid.Password = "username", "password"
	valid.Broker = kafkaBrokerURL(t, "kafkas://broker:9092")
	require.NoError(t, valid.Validate())
	for _, mechanism := range []string{"", "plain", "scram-sha-512", "scram-sha-256"} {
		valid.AuthMethod = mechanism
		require.NoError(t, valid.Validate())
		secureTransport, err := valid.transport()
		require.NoError(t, err)
		require.Equal(t, uint16(tls.VersionTLS12), secureTransport.TLS.MinVersion)
		require.False(t, secureTransport.TLS.InsecureSkipVerify)
		want := mechanism
		if want == "" {
			want = "scram-sha-512"
		}
		require.Equal(t, strings.ToUpper(want), secureTransport.SASL.Name())
	}
	valid.Username = "invalid\x07username"
	_, err = valid.transport()
	require.EqualError(t, err, "invalid Kafka SCRAM credentials; check LIVEPEER_SIGNER_KAFKA_USERNAME or --kafka-username-file and LIVEPEER_SIGNER_KAFKA_PASSWORD or --kafka-password-file")
}

func TestKafkaBrokerURLsAndDefaultPorts(t *testing.T) {
	for _, test := range []struct {
		name, broker, authMethod, want string
		credentials                    bool
		tls                            bool
	}{
		{name: "bare host defaults to TLS", broker: "broker", want: "broker:9093", tls: true},
		{name: "bare host explicit port", broker: "broker:19093", want: "broker:19093", tls: true},
		{name: "bare IPv4", broker: "127.0.0.1", want: "127.0.0.1:9093", tls: true},
		{name: "bare IPv4 explicit port", broker: "127.0.0.1:19093", want: "127.0.0.1:19093", tls: true},
		{name: "bare IPv6", broker: "[::1]", want: "[::1]:9093", tls: true},
		{name: "bare IPv6 explicit port", broker: "[::1]:19093", want: "[::1]:19093", tls: true},
		{name: "bare scoped IPv6", broker: "[fe80::1%25en0]:19093", want: "[fe80::1%en0]:19093", tls: true},
		{name: "bare SCRAM", broker: "broker", credentials: true, want: "broker:9096", tls: true},
		{name: "bare PLAIN", broker: "broker", authMethod: "plain", credentials: true, want: "broker:9094", tls: true},
		{name: "plaintext", broker: "kafka://broker", want: "broker:9092"},
		{name: "TLS only", broker: "kafkas://broker", want: "broker:9093", tls: true},
		{name: "default SCRAM", broker: "kafkas://broker", credentials: true, want: "broker:9096", tls: true},
		{name: "SCRAM 512 plaintext", broker: "kafka://broker", authMethod: "scram-sha-512", credentials: true, want: "broker:9096"},
		{name: "SCRAM 256 TLS", broker: "kafkas://broker", authMethod: "scram-sha-256", credentials: true, want: "broker:9096", tls: true},
		{name: "PLAIN TLS", broker: "kafkas://broker", authMethod: "plain", credentials: true, want: "broker:9094", tls: true},
		{name: "PLAIN plaintext", broker: "kafka://broker", authMethod: "plain", credentials: true, want: "broker:9094"},
		{name: "unused auth method", broker: "kafkas://broker", authMethod: "plain", want: "broker:9093", tls: true},
		{name: "explicit plaintext", broker: "kafka://broker:19092", want: "broker:19092"},
		{name: "explicit TLS", broker: "kafkas://broker:9194", want: "broker:9194", tls: true},
		{name: "explicit SCRAM", broker: "kafkas://broker:9196", credentials: true, want: "broker:9196", tls: true},
		{name: "explicit PLAIN", broker: "kafkas://broker:9095", authMethod: "plain", credentials: true, want: "broker:9095", tls: true},
		{name: "IPv6 omitted port", broker: "kafka://[::1]", want: "[::1]:9092"},
		{name: "IPv6 explicit port", broker: "kafkas://[::1]:19093", want: "[::1]:19093", tls: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := KafkaConfig{Broker: kafkaBrokerURL(t, test.broker), AuthMethod: test.authMethod}
			if test.credentials {
				c.Username, c.Password = "username", "password"
			}
			address, err := c.brokerAddress()
			require.NoError(t, err)
			require.Equal(t, test.want, address)
			transport, err := c.transport()
			require.NoError(t, err)
			require.Equal(t, test.tls, transport.TLS != nil)
			require.Equal(t, test.credentials, transport.SASL != nil)
		})
	}
	for _, broker := range []string{
		"", "https://broker:9092", "kafka://", "kafka://broker:",
		"kafka://broker:0", "kafka://broker:65536", "kafka://broker,other:9092",
		"kafka://user:password@broker", "kafka://broker/topic", "kafka://broker/",
		"kafka://broker?topic=signing", "kafka://broker?", "kafka://broker#fragment",
		"kafka://::1", "kafka://[not-an-ipv6-address]", "kafka://[127.0.0.1]",
		"broker:", "broker:65536", "broker:not-a-port", "::1", "[not-an-ipv6-address]",
	} {
		t.Run("reject "+broker, func(t *testing.T) {
			var address KafkaBroker
			err := address.UnmarshalText([]byte(broker))
			if err != nil {
				return
			}
			_, err = (KafkaConfig{Broker: address}).brokerAddress()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "password")
		})
	}
}

func TestOptionalKafkaBoaConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		config string
		args   []string
		env    map[string]string
		want   string
	}{
		{name: "omitted"},
		{name: "config without Kafka", config: "Orchestrators = []\n"},
		{name: "empty Kafka section", config: "[Kafka]\n", want: "broker"},
		{name: "config missing topic", config: "[Kafka]\nBroker = 'kafka://broker:9092'\n", want: "topic"},
		{name: "config missing broker", config: "[Kafka]\nTopic = 'signing'\n", want: "broker"},
		{name: "config defaults", config: "[Kafka]\nBroker = 'broker'\nTopic = 'signing'\n"},
		{name: "flag defaults", args: []string{"--kafka-broker", "127.0.0.1:9093", "--kafka-topic", "signing"}},
		{name: "env defaults", env: map[string]string{"BROKER": "[::1]:9093", "TOPIC": "signing"}},
		{name: "flag missing topic", args: []string{"--kafka-broker", "kafka://broker:9092"}, want: "topic"},
		{name: "flag missing broker", args: []string{"--kafka-topic", "signing"}, want: "broker"},
		{name: "outbox flag enables Kafka", args: []string{"--kafka-outbox-db", "signer/events.sqlite"}, want: "broker"},
		{name: "capacity flag enables Kafka", args: []string{"--kafka-outbox-max-bytes", "268435456"}, want: "broker"},
		{name: "outbox env enables Kafka", env: map[string]string{"OUTBOX_DB": "signer/events.sqlite"}, want: "broker"},
		{name: "outbox config enables Kafka", config: "[Kafka]\nOutboxDB = 'signer-events.sqlite'\n", want: "broker"},
		{name: "empty outbox rejected", args: []string{"--kafka-broker", "kafka://broker:9092", "--kafka-topic", "signing", "--kafka-outbox-db", ""}, want: "outbox"},
		{name: "invalid auth method rejected", args: []string{"--kafka-broker", "kafka://broker:9092", "--kafka-topic", "signing", "--kafka-auth-method", "invalid"}, want: "auth-method"},
		{name: "uppercase auth method rejected", config: "[Kafka]\nBroker = 'kafka://broker'\nTopic = 'signing'\nAuthMethod = 'SCRAM-SHA-512'\n", want: "auth-method"},
		{name: "empty auth method flag rejected", args: []string{"--kafka-broker", "kafka://broker", "--kafka-topic", "signing", "--kafka-auth-method", ""}, want: "auth-method"},
		{name: "empty auth method config rejected", config: "[Kafka]\nBroker = 'kafka://broker'\nTopic = 'signing'\nAuthMethod = ''\n", want: "auth-method"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LIVEPEER_SIGNER_DATA_DIR", t.TempDir())
			for name, value := range test.env {
				t.Setenv("LIVEPEER_SIGNER_KAFKA_"+name, value)
			}
			p, err := loadSignerParams(t, test.args, test.config)
			if test.want != "" {
				require.ErrorContains(t, err, test.want)
				return
			}
			require.NoError(t, err)
			if test.name == "omitted" || test.name == "config without Kafka" {
				require.Nil(t, p.Kafka)
				producer, err := openKafkaProducer(t.Context(), p.Kafka, "payer")
				require.NoError(t, err)
				require.Nil(t, producer)
				return
			}
			require.NotNil(t, p.Kafka)
			require.Equal(t, filepath.Join(p.DataDir, "signer/events.sqlite"), p.Kafka.OutboxDB)
			require.Equal(t, int64(268435456), p.Kafka.OutboxMaxBytes)
			require.Equal(t, "scram-sha-512", p.Kafka.AuthMethod)
			producer, err := openKafkaProducer(t.Context(), p.Kafka, "payer")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, producer.Close()) })
			info, err := os.Stat(p.Kafka.OutboxDB)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		})
	}
}

func TestNestedKafkaBoaConfigurationAndSecrets(t *testing.T) {
	type params struct {
		ConfigFile string       `configfile:"true" optional:"true"`
		Kafka      *KafkaConfig `optional:"true"`
	}
	dir := t.TempDir()
	usernameFile, passwordFile := filepath.Join(dir, "user"), filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(usernameFile, []byte("secret-user"), 0600))
	require.NoError(t, os.WriteFile(passwordFile, []byte("secret-password"), 0600))
	configPath := filepath.Join(dir, "signer.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("[Kafka]\nBroker = 'config:9092'\nTopic = 'signing'\nOutboxDB = 'events.sqlite'\nUsernameFile = %q\nPasswordFile = %q\n", usernameFile, passwordFile)), 0600))
	read := func(args ...string) (params, []byte, error) {
		var p params
		var dumped []byte
		cmd := boa.Cmd[params]{Params: &p, RejectUnknown: true, RawArgs: args,
			ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_SIGNER")),
			RunFuncCtxE: func(ctx *boa.HookContext, _ *params, _ *cobra.Command, _ []string) error {
				var err error
				dumped, err = ctx.DumpBytes(".toml", toml.Marshal)
				return err
			},
		}
		err := cmd.RunArgsE(args)
		return p, dumped, err
	}
	p, dumped, err := read("--config-file", configPath)
	require.NoError(t, err)
	require.Equal(t, "kafkas://config:9092", p.Kafka.Broker.String())
	require.Equal(t, "events.sqlite", p.Kafka.OutboxDB)
	require.Equal(t, int64(268435456), p.Kafka.OutboxMaxBytes)
	require.Equal(t, "secret-user", p.Kafka.Username)
	require.Equal(t, "secret-password", p.Kafka.Password)
	require.Equal(t, "scram-sha-512", p.Kafka.AuthMethod)
	transport, err := p.Kafka.transport()
	require.NoError(t, err)
	require.Equal(t, "SCRAM-SHA-512", transport.SASL.Name())
	require.NotContains(t, string(dumped), "secret-user")
	require.NotContains(t, string(dumped), "secret-password")
	require.Contains(t, string(dumped), "[Kafka]")
	require.Contains(t, string(dumped), `Broker = "kafkas://config:9092"`)
	var roundTrip params
	require.NoError(t, toml.Unmarshal(dumped, &roundTrip))
	require.Equal(t, p.Kafka.Broker.String(), roundTrip.Kafka.Broker.String())
	plainConfigPath := filepath.Join(dir, "plain.toml")
	require.NoError(t, os.WriteFile(plainConfigPath, []byte("[Kafka]\nBroker = 'kafkas://config'\nTopic = 'signing'\nAuthMethod = 'plain'\n"), 0600))
	plainParams, _, err := read("--config-file", plainConfigPath)
	require.NoError(t, err)
	require.Equal(t, "plain", plainParams.Kafka.AuthMethod)
	t.Setenv("LIVEPEER_SIGNER_KAFKA_BROKER", "env:9092")
	p, _, err = read("--config-file", configPath)
	require.NoError(t, err)
	require.Equal(t, "kafkas://env:9092", p.Kafka.Broker.String())
	p, _, err = read("--config-file", configPath, "--kafka-broker", "flag:9092")
	require.NoError(t, err)
	require.Equal(t, "kafkas://flag:9092", p.Kafka.Broker.String())
	t.Setenv("LIVEPEER_SIGNER_KAFKA_AUTH_METHOD", "scram-sha-256")
	p, _, err = read("--config-file", configPath)
	require.NoError(t, err)
	require.Equal(t, "scram-sha-256", p.Kafka.AuthMethod)
	p, _, err = read("--config-file", configPath, "--kafka-auth-method", "scram-sha-512")
	require.NoError(t, err)
	require.Equal(t, "scram-sha-512", p.Kafka.AuthMethod)
	_, _, err = read("--config-file", configPath, "--kafka-password", "forbidden-secret")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "forbidden-secret")
	bad := filepath.Join(dir, "bad.toml")
	require.NoError(t, os.WriteFile(bad, []byte("[Kafka]\nUsername = 'forbidden-secret'\n"), 0600))
	_, _, err = read("--config-file", bad)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "forbidden-secret")
	_, _, err = read("--kafka-outbox-max-bytes", "0")
	require.Error(t, err)
	var help bytes.Buffer
	root := Root(&help, &help)
	root.SetArgs([]string{"--help"})
	require.NoError(t, root.Execute())
	require.Contains(t, help.String(), "--kafka-broker")
	require.Contains(t, help.String(), "LIVEPEER_SIGNER_KAFKA_BROKER")
}
