// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	minio "github.com/storj/minio/cmd"
	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/common/fpath"
	"storj.io/common/rpc/rpcpool"
	"storj.io/gateway-mt/miniogw"
	"storj.io/gateway-mt/pkg/server"
	"storj.io/private/cfgstruct"
	"storj.io/private/process"
	"storj.io/uplink"
)

// GatewayFlags configuration flags.
type GatewayFlags struct {
	Server miniogw.ServerConfig
	Minio  miniogw.MinioConfig

	AuthURL            string `help:"Auth Service endpoint URL to return to clients" releaseDefault:"" devDefault:"http://localhost:8000" basic-help:"true"`
	AuthToken          string `help:"Auth Service security token to authenticate requests" releaseDefault:"" devDefault:"super-secret" basic-help:"true"`
	CertDir            string `help:"directory path to search for TLS certificates" default:"$CONFDIR/certs"`
	InsecureDisableTLS bool   `help:"listen using insecure connections" releaseDefault:"false" devDefault:"true"`
	DomainName         string `help:"comma-separated domain suffixes to serve on" releaseDefault:"" devDefault:"localhost" basic-help:"true"`

	Config
}

// ClientConfig is a configuration struct for the uplink that controls how
// to talk to the rest of the network.
type ClientConfig struct {
	DialTimeout time.Duration `help:"timeout for dials" default:"0h2m00s"`
}

// Config uplink configuration.
type Config struct {
	Client ClientConfig
}

var (
	// Error is the default gateway setup errs class.
	Error = errs.Class("gateway setup")
	// rootCmd represents the base gateway command when called without any subcommands.
	rootCmd = &cobra.Command{
		Use:   "gateway",
		Short: "The Storj client-side S3 gateway",
		Args:  cobra.OnlyValidArgs,
	}
	runCmd = &cobra.Command{
		Use:   "run",
		Short: "Run the classic S3-compatible gateway",
		RunE:  cmdRun,
	}
	runCfg GatewayFlags

	confDir string
)

func init() {
	defaultConfDir := fpath.ApplicationDir("storj", "gateway")
	cfgstruct.SetupFlag(zap.L(), rootCmd, &confDir, "config-dir", defaultConfDir, "main directory for gateway configuration")
	defaults := cfgstruct.DefaultsFlag(rootCmd)

	rootCmd.AddCommand(runCmd)
	process.Bind(runCmd, &runCfg, defaults, cfgstruct.ConfDir(confDir))

	rootCmd.PersistentFlags().BoolVar(new(bool), "advanced", false, "if used in with -h, print advanced flags help")
	cfgstruct.SetBoolAnnotation(rootCmd.PersistentFlags(), "advanced", cfgstruct.BasicHelpAnnotationName, true)
	cfgstruct.SetBoolAnnotation(rootCmd.PersistentFlags(), "config-dir", cfgstruct.BasicHelpAnnotationName, true)
	setUsageFunc(rootCmd)
}

func cmdRun(cmd *cobra.Command, args []string) (err error) {
	address := runCfg.Server.Address
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host == "" {
		address = net.JoinHostPort("127.0.0.1", port)
	}

	ctx, _ := process.Ctx(cmd)

	if err := process.InitMetricsWithHostname(ctx, zap.L(), nil); err != nil {
		zap.S().Warn("Failed to initialize telemetry batcher: ", err)
	}

	// setup environment variables for Minio
	validate := func(value, configName string) {
		if value == "" {
			err = errs.Combine(err, Error.New("required parameter --%s not set", configName))
		}
	}
	set := func(value, envName string) {
		err = errs.Combine(err, Error.Wrap(os.Setenv(envName, value)))
	}
	validate(runCfg.AuthToken, "auth-token")
	validate(runCfg.AuthURL, "auth-url")
	validate(runCfg.DomainName, "domain-name")
	set(runCfg.DomainName, "MINIO_DOMAIN") // MINIO_DOMAIN supports comma-separated domains.
	set("enable", "STORJ_AUTH_ENABLED")
	set("off", "MINIO_BROWSER")
	set("dummy-key-to-satisfy-minio", "MINIO_ACCESS_KEY")
	set("dummy-key-to-satisfy-minio", "MINIO_SECRET_KEY")
	if err != nil {
		return err
	}

	zap.S().Info("Starting Tardigrade S3 Gateway\n\n")
	zap.S().Infof("Endpoint: %s\n", address)
	zap.S().Info("Access key: use your Tardigrade Access Grant\n")
	zap.S().Info("Secret key: anything would work\n")

	return runCfg.Run(ctx, address)
}

// Run starts a Minio Gateway given proper config.
func (flags GatewayFlags) Run(ctx context.Context, address string) (err error) {
	// set object API handler
	gatewayLayer, err := flags.NewGateway(ctx)
	if err != nil {
		return err
	}

	// wire up domain names for Minio
	minio.HandleCommonEnvVars()
	// make Minio not use random ETags
	minio.SetGlobalCLI(false, true, false, address, true)
	store := minio.NewIAMStorjAuthStore(gatewayLayer, runCfg.AuthURL, runCfg.AuthToken)
	minio.SetObjectLayer(gatewayLayer)
	minio.InitCustomStore(store, "StorjAuthSys")

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	zap.S().Info("Starting Tardigrade S3 Gateway\n\n")
	zap.S().Infof("Endpoint: %s\n", address)
	zap.S().Info("Access key: use your Tardigrade Access Grant\n")
	zap.S().Info("Secret key: anything would work\n")

	// because existing configs contain most of these values, we don't have separate
	// parameter bindings for the non-Minio server
	var tlsConfig *tls.Config
	if !runCfg.InsecureDisableTLS {
		tlsConfig, err = server.LoadTLSConfigFromDir(runCfg.CertDir)
		if err != nil {
			return err
		}
	}
	s3 := server.New(listener, zap.L(), tlsConfig, address, strings.Split(runCfg.DomainName, ","))
	runError := s3.Run(ctx)
	closeError := s3.Close()
	return errs.Combine(runError, closeError)
}

// NewGateway creates a new minio Gateway.
func (flags GatewayFlags) NewGateway(ctx context.Context) (gw minio.ObjectLayer, err error) {
	config := flags.newUplinkConfig(ctx)
	pool := rpcpool.New(rpcpool.Options{
		Capacity:       10000,
		KeyCapacity:    2,
		IdleExpiration: 30 * time.Second,
	})

	return miniogw.NewGateway(config, pool, zap.L())
}

func (flags *GatewayFlags) newUplinkConfig(ctx context.Context) uplink.Config {
	// Transform the gateway config flags to the uplink config object
	config := uplink.Config{}
	config.DialTimeout = flags.Client.DialTimeout
	return config
}

/*	`setUsageFunc` is a bit unconventional but cobra didn't leave much room for
	extensibility here. `cmd.SetUsageTemplate` is fairly useless for our case without
	the ability to add to the template's function map (see: https://golang.org/pkg/text/template/#hdr-Functions).

	Because we can't alter what `cmd.Usage` generates, we have to edit it afterwards.
	In order to hook this function *and* get the usage string, we have to juggle the
	`cmd.usageFunc` between our hook and `nil`, so that we can get the usage string
	from the default usage func.
*/
func setUsageFunc(cmd *cobra.Command) {
	if findBoolFlagEarly("advanced") {
		return
	}

	reset := func() (set func()) {
		original := cmd.UsageFunc()
		cmd.SetUsageFunc(nil)

		return func() {
			cmd.SetUsageFunc(original)
		}
	}

	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		set := reset()
		usageStr := cmd.UsageString()
		defer set()

		usageScanner := bufio.NewScanner(bytes.NewBufferString(usageStr))

		var basicFlags []string
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			basic, ok := flag.Annotations[cfgstruct.BasicHelpAnnotationName]
			if ok && len(basic) == 1 && basic[0] == "true" {
				basicFlags = append(basicFlags, flag.Name)
			}
		})

		for usageScanner.Scan() {
			line := usageScanner.Text()
			trimmedLine := strings.TrimSpace(line)

			var flagName string
			if _, err := fmt.Sscanf(trimmedLine, "--%s", &flagName); err != nil {
				fmt.Println(line)
				continue
			}

			// TODO: properly filter flags with short names
			if !strings.HasPrefix(trimmedLine, "--") {
				fmt.Println(line)
			}

			for _, basicFlag := range basicFlags {
				if basicFlag == flagName {
					fmt.Println(line)
				}
			}
		}
		return nil
	})
}

func findBoolFlagEarly(flagName string) bool {
	for i, arg := range os.Args {
		arg := arg
		argHasPrefix := func(format string, args ...interface{}) bool {
			return strings.HasPrefix(arg, fmt.Sprintf(format, args...))
		}

		if !argHasPrefix("--%s", flagName) {
			continue
		}

		// NB: covers `--<flagName> false` usage
		if i+1 != len(os.Args) {
			next := os.Args[i+1]
			if next == "false" {
				return false
			}
		}

		if !argHasPrefix("--%s=false", flagName) {
			return true
		}
	}
	return false
}

func main() {
	process.Exec(rootCmd)
}
