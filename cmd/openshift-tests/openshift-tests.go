package main

import (
	"flag"
	goflag "flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/serviceability"
	"github.com/openshift/origin/pkg/monitor"
	"github.com/openshift/origin/pkg/monitor/resourcewatch/cmd"
	testginkgo "github.com/openshift/origin/pkg/test/ginkgo"
	"github.com/openshift/origin/pkg/version"
	exutil "github.com/openshift/origin/test/extended/util"
)

func main() {
	// KUBE_TEST_REPO_LIST is calculated during package initialization and prevents
	// proper mirroring of images referenced by tests. Clear the value and re-exec the
	// current process to ensure we can verify from a known state.
	if len(os.Getenv("KUBE_TEST_REPO_LIST")) > 0 {
		fmt.Fprintln(os.Stderr, "warning: KUBE_TEST_REPO_LIST may not be set when using openshift-tests and will be ignored")
		os.Setenv("KUBE_TEST_REPO_LIST", "")
		// resolve the call to execute since Exec() does not do PATH resolution
		if err := syscall.Exec(exec.Command(os.Args[0]).Path, os.Args, os.Environ()); err != nil {
			panic(fmt.Sprintf("%s: %v", os.Args[0], err))
		}
		return
	}

	logs.InitLogs()
	defer logs.FlushLogs()

	rand.Seed(time.Now().UTC().UnixNano())

	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	root := &cobra.Command{
		Long: templates.LongDesc(`
		OpenShift Tests

		This command verifies behavior of an OpenShift cluster by running remote tests against
		the cluster API that exercise functionality. In general these tests may be disruptive
		or require elevated privileges - see the descriptions of each test suite.
		`),
	}

	root.AddCommand(
		newRunCommand(),
		newRunUpgradeCommand(),
		newImagesCommand(),
		newRunTestCommand(),
		newRunMonitorCommand(),
		cmd.NewRunResourceWatchCommand(),
	)

	pflag.CommandLine = pflag.NewFlagSet("empty", pflag.ExitOnError)
	flag.CommandLine = flag.NewFlagSet("empty", flag.ExitOnError)
	exutil.InitStandardFlags()

	if err := func() error {
		defer serviceability.Profile(os.Getenv("OPENSHIFT_PROFILE")).Stop()
		return root.Execute()
	}(); err != nil {
		if ex, ok := err.(testginkgo.ExitError); ok {
			os.Exit(ex.Code)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newRunMonitorCommand() *cobra.Command {
	monitorOpt := &monitor.Options{
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
	cmd := &cobra.Command{
		Use:   "run-monitor",
		Short: "Continuously verify the cluster is functional",
		Long: templates.LongDesc(`
		Run a continuous verification process

		`),

		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return monitorOpt.Run()
		},
	}
	return cmd
}

type imagesOptions struct {
	Repository string
	Upstream   bool
	Verify     bool
}

func newImagesCommand() *cobra.Command {
	opt := &imagesOptions{}
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Gather images required for testing",
		Long: templates.LongDesc(fmt.Sprintf(`
		Creates a mapping to mirror test images to a private registry

		This command identifies the locations of all test images referenced by the test
		suite and outputs a mirror list for use with 'oc image mirror' to copy those images
		to a private registry. The list may be passed via file or standard input.

				$ openshift-tests images --to-repository private.com/test/repository > /tmp/mirror
				$ oc image mirror -f /tmp/mirror

		The 'run' and 'run-upgrade' subcommands accept '--from-repository' which will source
		required test images from your mirror.

		See the help for 'oc image mirror' for more about mirroring to disk or consult the docs
		for mirroring offline. You may use a file:// prefix in your '--to-repository', but when
		mirroring from disk to your offline repository you will have to construct the appropriate
		disk to internal registry statements yourself.

		By default, the test images are sourced from a public container image repository at
		%[1]s and are provided as-is for testing purposes only. Images are mirrored by the project
		to the public repository periodically.
		`, defaultTestImageMirrorLocation)),

		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opt.Verify {
				return verifyImages()
			}

			repository := opt.Repository
			var prefix string
			for _, validPrefix := range []string{"file://", "s3://"} {
				if strings.HasPrefix(repository, validPrefix) {
					repository = strings.TrimPrefix(repository, validPrefix)
					prefix = validPrefix
					break
				}
			}
			ref, err := reference.Parse(repository)
			if err != nil {
				return fmt.Errorf("--to-repository is not valid: %v", err)
			}
			if len(ref.Tag) > 0 || len(ref.ID) > 0 {
				return fmt.Errorf("--to-repository may not include a tag or image digest")
			}

			if err := verifyImages(); err != nil {
				return err
			}
			lines, err := createImageMirrorForInternalImages(prefix, ref, !opt.Upstream)
			if err != nil {
				return err
			}
			for _, line := range lines {
				fmt.Fprintln(os.Stdout, line)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opt.Upstream, "upstream", opt.Upstream, "Retrieve images from the default upstream location")
	cmd.Flags().StringVar(&opt.Repository, "to-repository", opt.Repository, "A container image repository to mirror to.")
	// this is a private flag for debugging only
	cmd.Flags().BoolVar(&opt.Verify, "verify", opt.Verify, "Verify the contents of the image mappings")
	cmd.Flags().MarkHidden("verify")
	return cmd
}

func newRunCommand() *cobra.Command {
	opt := &testginkgo.Options{
		Suites:         staticSuites,
		FromRepository: defaultTestImageMirrorLocation,
	}

	cmd := &cobra.Command{
		Use:   "run SUITE",
		Short: "Run a test suite",
		Long: templates.LongDesc(`
		Run a test suite against an OpenShift server

		This command will run one of the following suites against a cluster identified by the current
		KUBECONFIG file. See the suite description for more on what actions the suite will take.

		If you specify the --dry-run argument, the names of each individual test that is part of the
		suite will be printed, one per line. You may filter this list and pass it back to the run
		command with the --file argument. You may also pipe a list of test names, one per line, on
		standard input by passing "-f -".

		`) + testginkgo.SuitesString(opt.Suites, "\n\nAvailable test suites:\n\n"),

		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return mirrorToFile(opt, func() error {
				if !opt.DryRun {
					fmt.Fprintf(os.Stderr, "%s version: %s\n", filepath.Base(os.Args[0]), version.Get().String())
				}
				if err := verifyImages(); err != nil {
					return err
				}
				config, err := decodeProvider(opt.Provider, opt.DryRun, true)
				if err != nil {
					return err
				}
				opt.Provider = config.ToJSONString()
				opt.MatchFn = getProviderMatchFn(config)
				opt.SyntheticEventTests = pulledInvalidImages(opt.FromRepository)

				err = opt.Run(args)
				if !opt.DryRun && len(args) > 0 && strings.HasPrefix(args[0], "openshift/csi") {
					printStorageCapabilities(opt.Out)
				}
				return err
			})
		},
	}
	bindOptions(opt, cmd.Flags())
	return cmd
}

func newRunUpgradeCommand() *cobra.Command {
	opt := &testginkgo.Options{
		Suites:         upgradeSuites,
		FromRepository: defaultTestImageMirrorLocation,
	}
	upgradeOpt := &UpgradeOptions{}

	cmd := &cobra.Command{
		Use:   "run-upgrade SUITE",
		Short: "Run an upgrade suite",
		Long: templates.LongDesc(`
		Run an upgrade test suite against an OpenShift server

		This command will run one of the following suites against a cluster identified by the current
		KUBECONFIG file. See the suite description for more on what actions the suite will take.

		If you specify the --dry-run argument, the actions the suite will take will be printed to the
		output.

		Supported options:

		* abort-at=NUMBER - Set to a number between 0 and 100 to control the percent of operators
		at which to stop the current upgrade and roll back to the current version.
		* disrupt-reboot=POLICY - During upgrades, periodically reboot master nodes. If set to 'graceful'
		the reboot will allow the node to shut down services in an orderly fashion. If set to 'force' the
		machine will terminate immediately without clean shutdown.

		`) + testginkgo.SuitesString(opt.Suites, "\n\nAvailable upgrade suites:\n\n"),

		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return mirrorToFile(opt, func() error {
				if !opt.DryRun {
					fmt.Fprintf(os.Stderr, "%s version: %s\n", filepath.Base(os.Args[0]), version.Get().String())
				}
				if len(upgradeOpt.ToImage) == 0 {
					return fmt.Errorf("--to-image must be specified to run an upgrade test")
				}
				if err := verifyImages(); err != nil {
					return err
				}

				if len(args) > 0 {
					for _, suite := range opt.Suites {
						if suite.Name == args[0] {
							upgradeOpt.Suite = suite.Name
							upgradeOpt.JUnitDir = opt.JUnitDir
							value := upgradeOpt.ToEnv()
							if _, err := initUpgrade(value); err != nil {
								return err
							}
							opt.SuiteOptions = value
							break
						}
					}
				}

				config, err := decodeProvider(opt.Provider, opt.DryRun, true)
				if err != nil {
					return err
				}
				opt.Provider = config.ToJSONString()
				opt.MatchFn = getProviderMatchFn(config)
				opt.SyntheticEventTests = pulledInvalidImages(opt.FromRepository)
				return opt.Run(args)
			})
		},
	}

	bindOptions(opt, cmd.Flags())
	bindUpgradeOptions(upgradeOpt, cmd.Flags())
	return cmd
}

func newRunTestCommand() *cobra.Command {
	testOpt := &testginkgo.TestOptions{
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}

	cmd := &cobra.Command{
		Use:   "run-test NAME",
		Short: "Run a single test by name",
		Long: templates.LongDesc(`
		Execute a single test

		This executes a single test by name. It is used by the run command during suite execution but may also
		be used to test in isolation while developing new tests.
		`),

		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := verifyImagesWithoutEnv(); err != nil {
				return err
			}
			upgradeOpts, err := initUpgrade(os.Getenv("TEST_SUITE_OPTIONS"))
			if err != nil {
				return err
			}
			config, err := decodeProvider(os.Getenv("TEST_PROVIDER"), testOpt.DryRun, false)
			if err != nil {
				return err
			}
			if err := initializeTestFramework(exutil.TestContext, config, testOpt.DryRun); err != nil {
				return err
			}
			exutil.TestContext.ReportDir = upgradeOpts.JUnitDir
			exutil.WithCleanup(func() { err = testOpt.Run(args) })
			return err
		},
	}
	cmd.Flags().BoolVar(&testOpt.DryRun, "dry-run", testOpt.DryRun, "Print the test to run without executing them.")
	return cmd
}

// mirrorToFile ensures a copy of all output goes to the provided OutFile, including
// any error returned from fn. The function returns fn() or any error encountered while
// attempting to open the file.
func mirrorToFile(opt *testginkgo.Options, fn func() error) error {
	if opt.Out == nil {
		opt.Out = os.Stdout
	}
	if opt.ErrOut == nil {
		opt.ErrOut = os.Stderr
	}
	if len(opt.OutFile) == 0 {
		return fn()
	}

	f, err := os.OpenFile(opt.OutFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	opt.Out = io.MultiWriter(opt.Out, f)
	opt.ErrOut = io.MultiWriter(opt.ErrOut, f)
	exitErr := fn()
	if exitErr != nil {
		fmt.Fprintf(f, "error: %s", exitErr)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(opt.ErrOut, "error: Unable to close output file\n")
	}
	return exitErr
}

func bindOptions(opt *testginkgo.Options, flags *pflag.FlagSet) {
	flags.BoolVar(&opt.DryRun, "dry-run", opt.DryRun, "Print the tests to run without executing them.")
	flags.BoolVar(&opt.PrintCommands, "print-commands", opt.PrintCommands, "Print the sub-commands that would be executed instead.")
	flags.StringVar(&opt.JUnitDir, "junit-dir", opt.JUnitDir, "The directory to write test reports to.")
	flags.StringVar(&opt.Provider, "provider", opt.Provider, "The cluster infrastructure provider. Will automatically default to the correct value.")
	flags.StringVarP(&opt.TestFile, "file", "f", opt.TestFile, "Create a suite from the newline-delimited test names in this file.")
	flags.StringVar(&opt.Regex, "run", opt.Regex, "Regular expression of tests to run.")
	flags.StringVarP(&opt.OutFile, "output-file", "o", opt.OutFile, "Write all test output to this file.")
	flags.IntVar(&opt.Count, "count", opt.Count, "Run each test a specified number of times. Defaults to 1 or the suite's preferred value. -1 will run forever.")
	flags.BoolVar(&opt.FailFast, "fail-fast", opt.FailFast, "If a test fails, exit immediately.")
	flags.DurationVar(&opt.Timeout, "timeout", opt.Timeout, "Set the maximum time a test can run before being aborted. This is read from the suite by default, but will be 10 minutes otherwise.")
	flags.BoolVar(&opt.IncludeSuccessOutput, "include-success", opt.IncludeSuccessOutput, "Print output from successful tests.")
	flags.IntVar(&opt.Parallelism, "max-parallel-tests", opt.Parallelism, "Maximum number of tests running in parallel. 0 defaults to test suite recommended value, which is different in each suite.")
	flags.StringVar(&opt.FromRepository, "from-repository", opt.FromRepository, "A container image repository to retrieve test images from.")
}
