package cli_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/datawire/ambassador/pkg/dtest"
	"github.com/datawire/dlib/dexec"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli"
	"github.com/telepresenceio/telepresence/v2/pkg/version"
)

// serviceCount is the number of interceptable services that gets installed
// in the cluster and later intercepted
const serviceCount = 3

func TestTelepresence(t *testing.T) {
	dtest.WithMachineLock(func() {
		suite.Run(t, new(telepresenceSuite))
	})
}

type telepresenceSuite struct {
	suite.Suite
	testVersion          string
	namespace            string
	managerTestNamespace string
}

func (ts *telepresenceSuite) SetupSuite() {
	// Check that the "ko" program exists, and adjust PATH as necessary.
	if info, err := os.Stat("../../../tools/bin/ko"); err != nil || !info.Mode().IsRegular() || (info.Mode().Perm()&0100) == 0 {
		ts.Fail("it looks like the ./tools/bin/ko executable wasn't built; be sure to build it with `make` before running `go test`!")
		return
	}
	toolbindir, err := filepath.Abs("../../../tools/bin")
	if !ts.NoError(err) {
		return
	}
	_ = os.Chdir("../../..")

	os.Setenv("PATH", toolbindir+":"+os.Getenv("PATH"))

	// Remove very verbose output from DTEST initialization
	log.SetOutput(io.Discard)

	ts.testVersion = fmt.Sprintf("v2.0.0-gotest.%d", os.Getpid())
	ts.namespace = fmt.Sprintf("telepresence-%d", os.Getpid())
	ts.managerTestNamespace = fmt.Sprintf("ambassador-%d", os.Getpid())

	version.Version = ts.testVersion

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		executable, err := ts.buildExecutable()
		ts.NoError(err)
		client.SetExe(executable)
	}()

	_ = os.Remove(client.ConnectorSocketName)
	err = run("sudo", "true")
	ts.NoError(err, "acquire privileges")

	registry := dtest.DockerRegistry()
	os.Setenv("KO_DOCKER_REPO", registry)
	os.Setenv("TELEPRESENCE_REGISTRY", registry)
	os.Setenv("TELEPRESENCE_MANAGER_NAMESPACE", ts.managerTestNamespace)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := ts.publishManager()
		ts.NoError(err)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		kubeconfig := dtest.Kubeconfig()
		os.Setenv("DTEST_KUBECONFIG", kubeconfig)
		os.Setenv("KUBECONFIG", kubeconfig)
		err = run("kubectl", "create", "namespace", ts.namespace)
		ts.NoError(err)
	}()
	wg.Wait()

	wg.Add(serviceCount)
	for i := 0; i < serviceCount; i++ {
		i := i
		go func() {
			defer wg.Done()
			err = ts.applyEchoService(fmt.Sprintf("hello-%d", i))
			ts.NoError(err)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		err = ts.applyApp("with-probes")
		ts.NoError(err)
	}()
	wg.Wait()

	// Ensure that no telepresence is running when the tests start
	_, _ = telepresence(ts.T(), "quit")

	// Also ensure that telepresence is not logged in
	_, _ = telepresence(ts.T(), "logout")
}

func (ts *telepresenceSuite) TearDownSuite() {
	_ = run("kubectl", "delete", "namespace", ts.namespace)
	_ = run("kubectl", "delete", "namespace", ts.managerTestNamespace)
}

func (ts *telepresenceSuite) TestA_WithNoDaemonRunning() {
	ts.Run("Version", func() {
		stdout, stderr := telepresence(ts.T(), "version")
		ts.Empty(stderr)
		ts.Contains(stdout, fmt.Sprintf("Client %s", client.DisplayVersion()))
	})
	ts.Run("Status", func() {
		out, _ := telepresence(ts.T(), "status")
		ts.Contains(out, "The telepresence daemon has not been started")
	})

	ts.Run("Connect using invalid KUBECONFIG", func() {
		ts.Run("Reports config error and exits", func() {
			kubeConfig := os.Getenv("KUBECONFIG")
			defer os.Setenv("KUBECONFIG", kubeConfig)
			os.Setenv("KUBECONFIG", "/dev/null")
			stdout, stderr := telepresence(ts.T(), "connect")
			ts.Contains(stderr, "kubeconfig has no context definition")
			ts.Contains(stdout, "Launching Telepresence Daemon")
			ts.Contains(stdout, "Daemon quitting")
		})
	})

	ts.Run("Connect with non existing context", func() {
		ts.Run("Reports connect error and exits", func() {
			stdout, stderr := telepresence(ts.T(), "connect", "--context", "not-likely-to-exist")
			ts.Contains(stderr, `"not-likely-to-exist" does not exist`)
			ts.Contains(stdout, "Launching Telepresence Daemon")
			ts.Contains(stdout, "Daemon quitting")
		})
	})

	ts.Run("Connect with a command", func() {
		ts.Run("Connects, executes the command, and then exits", func() {
			stdout, stderr := telepresence(ts.T(), "connect", "--", client.GetExe(), "status")
			ts.Empty(stderr)
			ts.Contains(stdout, "Launching Telepresence Daemon")
			ts.Contains(stdout, "Connected to context")
			ts.Contains(stdout, "Context:")
			ts.Regexp(`Proxy:\s+ON`, stdout)
			ts.Contains(stdout, "Daemon quitting")
		})
	})
}

func (ts *telepresenceSuite) TestB_Connected() {
	suite.Run(ts.T(), &connectedSuite{namespace: ts.namespace})
}

func (ts *telepresenceSuite) TestC_Uninstall() {
	ts.Run("Uninstalls agent on given deployment", func() {
		agentName := func() (string, error) {
			return ts.kubectlOut("get", "deploy", "with-probes", "-o",
				`jsonpath={.spec.template.spec.containers[?(@.name=="traffic-agent")].name}`)
		}
		stdout, err := agentName()
		ts.NoError(err)
		ts.Equal("traffic-agent", stdout)
		_, stderr := telepresence(ts.T(), "uninstall", "--namespace", ts.namespace, "--agent", "with-probes")
		ts.Empty(stderr)
		defer telepresence(ts.T(), "quit")
		ts.Eventually(
			// condition
			func() bool {
				stdout, _ := agentName()
				return stdout == ""
			},
			5*time.Second,        // waitFor
			500*time.Millisecond, // polling interval
		)
	})

	ts.Run("Uninstalls all agents", func() {
		agentNames := func() (string, error) {
			return ts.kubectlOut("get", "deploy", "-o",
				`jsonpath={.items[*].spec.template.spec.containers[?(@.name=="traffic-agent")].name}`)
		}
		stdout, err := agentNames()
		ts.NoError(err)
		ts.Equal(serviceCount, len(strings.Split(stdout, " ")))
		_, stderr := telepresence(ts.T(), "uninstall", "--namespace", ts.namespace, "--all-agents")
		ts.Empty(stderr)
		defer telepresence(ts.T(), "quit")
		ts.Eventually(
			func() bool {
				stdout, _ := agentNames()
				return stdout == ""
			},
			5*time.Second,        // waitFor
			500*time.Millisecond, // polling interval
		)
	})

	ts.Run("Uninstalls the traffic manager and quits", func() {
		names := func() (string, error) {
			return ts.kubectlOut("get",
				"--namespace", ts.managerTestNamespace,
				"svc,deploy", "traffic-manager",
				"--ignore-not-found",
				"-o", "jsonpath={.items[*].metadata.name}")
		}
		stdout, err := names()
		ts.NoError(err)
		ts.Equal(2, len(strings.Split(stdout, " "))) // The service and the deployment
		stdout, stderr := telepresence(ts.T(), "uninstall", "--everything")
		ts.Empty(stderr)
		ts.Contains(stdout, "Daemon quitting")
		ts.Eventually(
			func() bool {
				stdout, _ := names()
				return stdout == ""
			},
			5*time.Second,        // waitFor
			500*time.Millisecond, // polling interval
		)
	})
}

type connectedSuite struct {
	suite.Suite
	namespace string
}

func (cs *connectedSuite) SetupSuite() {
	stdout, stderr := telepresence(cs.T(), "connect")
	cs.Empty(stderr)
	cs.Contains(stdout, "Connected to context")

	// Give outbound interceptor 15 seconds to kick in.
	cs.Eventually(
		// condition
		func() bool {
			stdout, _ := telepresence(cs.T(), "status")
			return regexp.MustCompile(`Proxy:\s+ON`).FindString(stdout) != ""
		},
		15*time.Second, // waitFor
		time.Second,    // polling interval
		"Timeout waiting for network overrides to establish", // msg
	)
}

func (cs *connectedSuite) TearDownSuite() {
	stdout, stderr := telepresence(cs.T(), "quit")
	cs.Empty(stderr)
	cs.Contains(stdout, "quitting")
	time.Sleep(time.Second) // Allow some time for processes to die and sockets to vanish
}

func (cs *connectedSuite) TestA_ReportsVersionFromDaemon() {
	stdout, stderr := telepresence(cs.T(), "version")
	cs.Empty(stderr)
	vs := client.DisplayVersion()
	cs.Contains(stdout, fmt.Sprintf("Client %s", vs))
	cs.Contains(stdout, fmt.Sprintf("Daemon %s", vs))
}

func (cs *connectedSuite) TestB_ReportsStatusAsConnected() {
	stdout, stderr := telepresence(cs.T(), "status")
	cs.Empty(stderr)
	cs.Contains(stdout, "Context:")
}

func (cs *connectedSuite) TestC_ProxiesOutboundTraffic() {
	for i := 0; i < serviceCount; i++ {
		svc := fmt.Sprintf("hello-%d.%s", i, cs.namespace)
		expectedOutput := fmt.Sprintf("Request served by hello-%d", i)
		cs.Eventually(
			// condition
			func() bool {
				cs.T().Logf("trying %q...", "http://"+svc)
				resp, err := http.Get("http://" + svc)
				if err != nil {
					cs.T().Log(err)
					return false
				}
				defer resp.Body.Close()
				cs.T().Logf("status code: %v", resp.StatusCode)
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					cs.T().Log(err)
					return false
				}
				cs.T().Logf("body: %q", body)
				return strings.Contains(string(body), expectedOutput)
			},
			15*time.Second, // waitfor
			3*time.Second,  // polling interval
			`body of %q contains %q`, "http://"+svc, expectedOutput,
		)
	}
}

func (cs *connectedSuite) TestD_Intercepted() {
	suite.Run(cs.T(), &interceptedSuite{namespace: cs.namespace})
}

func (cs *connectedSuite) TestE_SuccessfullyInterceptsDeploymentWithProbes() {
	defer telepresence(cs.T(), "leave", "with-probes-"+cs.namespace)

	stdout, stderr := telepresence(cs.T(), "intercept", "--namespace", cs.namespace, "--mount", "false", "with-probes", "--port", "9090")
	cs.Empty(stderr)
	cs.Contains(stdout, "Using deployment with-probes")
	stdout, stderr = telepresence(cs.T(), "list", "--namespace", cs.namespace, "--intercepts")
	cs.Empty(stderr)
	cs.Contains(stdout, "with-probes: intercepted")
}

func (cs *connectedSuite) TestF_LocalOnlyIntercept() {
	cs.Run("intercept can be established", func() {
		stdout, stderr := telepresence(cs.T(), "intercept", "--namespace", cs.namespace, "--local-only", "mylocal")
		cs.Empty(stdout)
		cs.Empty(stderr)
	})

	cs.Run("is included in list output", func() {
		// list includes local intercept
		stdout, stderr := telepresence(cs.T(), "list", "--namespace", cs.namespace, "--intercepts")
		cs.Empty(stderr)
		cs.Contains(stdout, "mylocal: local-only intercept")
	})

	cs.Run("makes services reachable using unqualified name", func() {
		ctx := dlog.NewTestContext(cs.T(), false)

		// service can be resolve with unqualified name
		ip, err := net.DefaultResolver.LookupHost(ctx, "hello-0")
		cs.NoError(err)
		cs.True(len(ip) == 1)
	})

	cs.Run("leaving renders services unavailable using unqualified name", func() {
		stdout, stderr := telepresence(cs.T(), "leave", "mylocal")
		cs.Empty(stdout)
		cs.Empty(stderr)
		ctx := dlog.NewTestContext(cs.T(), false)
		cs.Eventually(func() bool {
			_, err := net.DefaultResolver.LookupHost(ctx, "hello-0")
			return err != nil
		}, 3*time.Second, 300*time.Millisecond)
	})
}

type interceptedSuite struct {
	suite.Suite
	namespace  string
	intercepts []string
	services   []*http.Server
}

func (is *interceptedSuite) SetupSuite() {
	is.intercepts = make([]string, 0, serviceCount)
	is.services = make([]*http.Server, 0, serviceCount)

	is.Run("all intercepts ready", func() {
		rxs := make([]*regexp.Regexp, serviceCount)
		for i := 0; i < serviceCount; i++ {
			rxs[i] = regexp.MustCompile(fmt.Sprintf("hello-%d\\s*:\\s+ready to intercept", i))
		}
		require.Eventually(is.T(),
			// condition
			func() bool {
				stdout, _ := telepresence(is.T(), "list", "--namespace", is.namespace)
				is.T().Log(stdout)
				for i := 0; i < serviceCount; i++ {
					if !rxs[i].MatchString(stdout) {
						return false
					}
				}
				return true
			},
			15*time.Second, // waitFor
			3*time.Second,  // polling interval
			`telepresence list reports all agents`,
		)
	})

	is.Run("adding intercepts", func() {
		for i := 0; i < serviceCount; i++ {
			svc := fmt.Sprintf("hello-%d", i)
			port := strconv.Itoa(9000 + i)
			stdout, stderr := telepresence(is.T(), "intercept", "--namespace", is.namespace, "--mount", "false", svc, "--port", port)
			is.Empty(stderr)
			is.intercepts = append(is.intercepts, svc)
			is.Contains(stdout, "Using deployment "+svc)
		}
	})

	is.Run("starting http servers", func() {
		for i := 0; i < serviceCount; i++ {
			svc := fmt.Sprintf("hello-%d", i)
			port := strconv.Itoa(9000 + i)
			srv := &http.Server{Addr: ":" + port, Handler: http.NewServeMux()}
			go func() {
				srv.Handler.(*http.ServeMux).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprintf(w, "%s from intercept at %s", svc, r.URL.Path)
				})
				is.services = append(is.services, srv)
				err := srv.ListenAndServe()
				is.Equal(http.ErrServerClosed, err)
			}()
		}
	})
}

func (is *interceptedSuite) TearDownSuite() {
	for _, svc := range is.intercepts {
		stdout, stderr := telepresence(is.T(), "leave", svc+"-"+is.namespace)
		is.Empty(stderr)
		is.Empty(stdout)
	}
	for _, srv := range is.services {
		_ = srv.Shutdown(context.Background())
	}
	time.Sleep(time.Second) // Allow some time for processes to die and intercepts to vanish
}

func (is *interceptedSuite) TestA_VerifyingResponsesFromInterceptor() {
	for i := 0; i < serviceCount; i++ {
		svc := fmt.Sprintf("hello-%d", i)
		expectedOutput := fmt.Sprintf("%s from intercept at /", svc)
		is.Eventually(
			// condition
			func() bool {
				is.T().Logf("trying %q...", "http://"+svc)
				resp, err := http.Get("http://" + svc)
				if err != nil {
					is.T().Log(err)
					return false
				}
				defer resp.Body.Close()
				is.T().Logf("status code: %v", resp.StatusCode)
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					is.T().Log(err)
					return false
				}
				is.T().Logf("body: %q", body)
				return string(body) == expectedOutput
			},
			15*time.Second, // waitFor
			3*time.Second,  // polling interval
			`body of %q equals %q`, "http://"+svc, expectedOutput,
		)
	}
}

func (is *interceptedSuite) TestB_ListingActiveIntercepts() {
	stdout, stderr := telepresence(is.T(), "--namespace", is.namespace, "list", "--intercepts")
	is.Empty(stderr)
	for i := 0; i < serviceCount; i++ {
		is.Contains(stdout, fmt.Sprintf("hello-%d: intercepted", i))
	}
}

func (ts *telepresenceSuite) applyApp(name string) error {
	err := ts.kubectl("apply", "-f", fmt.Sprintf("k8s/%s.yaml", name))
	if err != nil {
		return fmt.Errorf("failed to deploy %s: %v", name, err)
	}
	return ts.waitForService(name)
}

func (ts *telepresenceSuite) applyEchoService(name string) error {
	err := ts.kubectl("create", "deploy", name, "--image", "jmalloc/echo-server:0.1.0")
	if err != nil {
		return fmt.Errorf("failed to create deployment %s: %v", name, err)
	}
	err = ts.kubectl("expose", "deploy", name, "--port", "80", "--target-port", "8080")
	if err != nil {
		return fmt.Errorf("failed to expose deployment %s: %v", name, err)
	}
	return ts.waitForService(name)
}

func (ts *telepresenceSuite) waitForService(name string) error {
	for i := 0; i < 120; i++ {
		time.Sleep(time.Second)
		err := ts.kubectl("run", "curl-from-cluster", "--rm", "-it",
			"--image=docker.io/pstauffer/curl", "--restart=Never", "--",
			"curl", "--silent", "--output", "/dev/null",
			fmt.Sprintf("http://%s.%s", name, ts.namespace),
		)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for %s service", name)
}

func (ts *telepresenceSuite) kubectl(args ...string) error {
	return run(append([]string{"kubectl", "--namespace", ts.namespace}, args...)...)
}

func (ts *telepresenceSuite) kubectlOut(args ...string) (string, error) {
	return output(append([]string{"kubectl", "--namespace", ts.namespace}, args...)...)
}

func (ts *telepresenceSuite) publishManager() error {
	ctx := dlog.NewTestContext(ts.T(), true)
	cmd := dexec.CommandContext(ctx, "make", "push-image")
	cmd.Env = append(os.Environ(),
		"TELEPRESENCE_VERSION="+ts.testVersion,
		"TELEPRESENCE_REGISTRY="+dtest.DockerRegistry())
	if err := cmd.Run(); err != nil {
		return client.RunError(err)
	}
	return nil
}

func (ts *telepresenceSuite) buildExecutable() (string, error) {
	executable := filepath.Join("build-output", "bin", "/telepresence")
	return executable, run("go", "build", "-ldflags",
		fmt.Sprintf("-X=github.com/telepresenceio/telepresence/v2/pkg/version.Version=%s", ts.testVersion),
		"-o", executable, "./cmd/telepresence")
}

func run(args ...string) error {
	return client.RunError(exec.Command(args[0], args[1:]...).Run())
}

func output(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).Output()
	return string(out), client.RunError(err)
}

// telepresence executes the CLI command in-process
func telepresence(t testing.TB, args ...string) (string, string) {
	ctx := dlog.NewTestContext(t, false)

	dlog.Infof(ctx, "running command: %q", append([]string{"telepresence"}, args...))

	cmd := cli.Command(ctx)

	stdout := new(strings.Builder)
	cmd.SetOut(io.MultiWriter(
		stdout,
		dlog.StdLogger(dlog.WithField(ctx, "stream", "stdout"), dlog.LogLevelInfo).Writer(),
	))

	stderr := new(strings.Builder)
	cmd.SetErr(io.MultiWriter(
		stderr,
		dlog.StdLogger(dlog.WithField(ctx, "stream", "stderr"), dlog.LogLevelInfo).Writer(),
	))

	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
	}

	dlog.Infof(ctx, "command terminated %q", append([]string{"telepresence"}, args...))
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())
}
