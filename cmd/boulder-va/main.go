package notmain

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/letsencrypt/boulder/bdns"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	"github.com/letsencrypt/boulder/va"
	vapb "github.com/letsencrypt/boulder/va/proto"
)

type Config struct {
	VA struct {
		cmd.ServiceConfig

		UserAgent string

		IssuerDomain string

		// DNSTries is the number of times to try a DNS query (that has a temporary error)
		// before giving up. May be short-circuited by deadlines. A zero value
		// will be turned into 1.
		DNSTries                  int
		DNSResolver               string `validate:"required"`
		DNSTimeout                string
		DNSAllowLoopbackAddresses bool

		RemoteVAs                   []cmd.GRPCClientConfig `validate:"omitempty,dive"`
		MaxRemoteValidationFailures int

		Features map[string]bool

		AccountURIPrefixes []string
	}

	Syslog        cmd.SyslogConfig
	OpenTelemetry cmd.OpenTelemetryConfig

	// TODO(#6716): Remove Config.Common once all instances of it have been
	// removed from production config files.
	Common struct {
		// DEPRECATED: Use VA.DNSTimeout instead.
		DNSTimeout string
		// DEPRECATED: Use VA.DNSAllowLoopbackAddresses instead.
		DNSAllowLoopbackAddresses bool
	}
}

func main() {
	grpcAddr := flag.String("addr", "", "gRPC listen address override")
	debugAddr := flag.String("debug-addr", "", "Debug server address override")
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	var c Config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	err = features.Set(c.VA.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	if *grpcAddr != "" {
		c.VA.GRPC.Address = *grpcAddr
	}
	if *debugAddr != "" {
		c.VA.DebugAddr = *debugAddr
	}

	scope, logger, shutdown := cmd.StatsAndLogging(c.Syslog, c.OpenTelemetry, c.VA.DebugAddr)
	defer shutdown(context.Background())
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	var dnsTimeout time.Duration
	if c.VA.DNSTimeout != "" {
		dnsTimeout, err = time.ParseDuration(c.VA.DNSTimeout)
	} else {
		dnsTimeout, err = time.ParseDuration(c.Common.DNSTimeout)
	}
	cmd.FailOnError(err, "Couldn't parse DNS timeout")
	dnsTries := c.VA.DNSTries
	if dnsTries < 1 {
		dnsTries = 1
	}
	clk := cmd.Clock()

	var servers bdns.ServerProvider
	if c.VA.DNSResolver == "" {
		cmd.Fail("Config key 'dnsresolver' is required")
	}
	servers, err = bdns.StartDynamicProvider(c.VA.DNSResolver, 60*time.Second)
	cmd.FailOnError(err, "Couldn't start dynamic DNS server resolver")
	defer servers.Stop()

	var resolver bdns.Client
	if !(c.VA.DNSAllowLoopbackAddresses || c.Common.DNSAllowLoopbackAddresses) {
		resolver = bdns.New(
			dnsTimeout,
			servers,
			scope,
			clk,
			dnsTries,
			logger)
	} else {
		resolver = bdns.NewTest(
			dnsTimeout,
			servers,
			scope,
			clk,
			dnsTries,
			logger)
	}

	tlsConfig, err := c.VA.TLS.Load()
	cmd.FailOnError(err, "tlsConfig config")

	var remotes []va.RemoteVA
	if len(c.VA.RemoteVAs) > 0 {
		for _, rva := range c.VA.RemoteVAs {
			rva := rva
			vaConn, err := bgrpc.ClientSetup(&rva, tlsConfig, scope, clk)
			cmd.FailOnError(err, "Unable to create remote VA client")
			remotes = append(
				remotes,
				va.RemoteVA{
					VAClient: vapb.NewVAClient(vaConn),
					Address:  rva.ServerAddress,
				},
			)
		}
	}

	vai, err := va.NewValidationAuthorityImpl(
		resolver,
		remotes,
		c.VA.MaxRemoteValidationFailures,
		c.VA.UserAgent,
		c.VA.IssuerDomain,
		scope,
		clk,
		logger,
		c.VA.AccountURIPrefixes)
	cmd.FailOnError(err, "Unable to create VA server")

	start, err := bgrpc.NewServer(c.VA.GRPC).Add(
		&vapb.VA_ServiceDesc, vai).Add(
		&vapb.CAA_ServiceDesc, vai).Build(tlsConfig, scope, clk)
	cmd.FailOnError(err, "Unable to setup VA gRPC server")

	cmd.FailOnError(start(), "VA gRPC service failed")
}

func init() {
	cmd.RegisterCommand("boulder-va", main, &cmd.ConfigValidator{Config: &Config{}})
	// We register under two different names, because it's convenient for the
	// remote VAs to show up under a different program name when looking at logs.
	cmd.RegisterCommand("boulder-remoteva", main, &cmd.ConfigValidator{Config: &Config{}})
}
