/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func run(deploy deployer, o options) error {
	if o.checkSkew {
		os.Setenv("KUBECTL", "./cluster/kubectl.sh --match-server-version")
	} else {
		os.Setenv("KUBECTL", "./cluster/kubectl.sh")
	}
	os.Setenv("KUBE_CONFIG_FILE", "config-test.sh")
	// force having batch/v2alpha1 always on for e2e tests
	os.Setenv("KUBE_RUNTIME_CONFIG", "batch/v2alpha1=true")

	dump := o.dump
	if dump != "" {
		if !filepath.IsAbs(dump) {
			// Directory may change
			if wd, err := os.Getwd(); err != nil {
				return fmt.Errorf("failed to os.Getwd(): %v", err)
			} else {
				dump = filepath.Join(wd, dump)
			}
		}
	}

	if o.up {
		if o.federation {
			if err := xmlWrap("Federation TearDown Previous", FedDown); err != nil {
				return fmt.Errorf("error tearing down previous federation control plane: %v", err)
			}
		}
		if err := xmlWrap("TearDown Previous", deploy.Down); err != nil {
			return fmt.Errorf("error tearing down previous cluster: %s", err)
		}
	}

	var err error
	var errs []error

	// Ensures that the cleanup/down action is performed exactly once.
	var (
		downDone           bool = false
		federationDownDone bool = false
	)

	var (
		beforeResources []byte
		upResources     []byte
		downResources   []byte
		afterResources  []byte
	)

	if o.checkLeaks {
		errs = appendError(errs, xmlWrap("ListResources Before", func() error {
			beforeResources, err = ListResources()
			return err
		}))
	}

	if o.up {
		// If we tried to bring the cluster up, make a courtesy
		// attempt to bring it down so we're not leaving resources around.
		if o.down {
			defer xmlWrap("Deferred TearDown", func() error {
				if !downDone {
					return deploy.Down()
				}
				return nil
			})
			// Deferred statements are executed in last-in-first-out order, so
			// federation down defer must appear after the cluster teardown in
			// order to execute that before cluster teardown.
			if o.federation {
				defer xmlWrap("Deferred Federation TearDown", func() error {
					if !federationDownDone {
						return FedDown()
					}
					return nil
				})
			}
		}
		// Start the cluster using this version.
		if err := xmlWrap("Up", deploy.Up); err != nil {
			if dump != "" {
				xmlWrap("DumpClusterLogs (--up failed)", func() error {
					// This frequently means the cluster does not exist.
					// Thus DumpClusterLogs() typically fails.
					// Therefore always return null for this scenarios.
					// TODO(fejta): report a green E in testgrid if it errors.
					DumpClusterLogs(dump)
					return nil
				})
			}
			return fmt.Errorf("starting e2e cluster: %s", err)
		}
		if o.federation {
			if err := xmlWrap("Federation Up", FedUp); err != nil {
				xmlWrap("DumpFederationLogs", func() error {
					return DumpFederationLogs(dump)
				})
				return fmt.Errorf("error starting federation: %s", err)
			}
		}
		if dump != "" {
			errs = appendError(errs, xmlWrap("list nodes", func() error {
				return listNodes(dump)
			}))
		}
	}

	if o.checkLeaks {
		errs = appendError(errs, xmlWrap("ListResources Up", func() error {
			upResources, err = ListResources()
			return err
		}))
	}

	if o.upgradeArgs != "" {
		errs = appendError(errs, xmlWrap("UpgradeTest", func() error {
			return UpgradeTest(o.upgradeArgs, dump, o.checkSkew)
		}))
	}

	if o.test {
		errs = appendError(errs, xmlWrap("get kubeconfig", deploy.SetupKubecfg))
		errs = appendError(errs, xmlWrap("kubectl version", func() error {
			return finishRunning(exec.Command("./cluster/kubectl.sh", "version", "--match-server-version=false"))
		}))
		if o.skew {
			errs = appendError(errs, xmlWrap("SkewTest", func() error {
				return SkewTest(o.testArgs, dump, o.checkSkew)
			}))
		} else {
			if err := xmlWrap("IsUp", deploy.IsUp); err != nil {
				errs = appendError(errs, err)
			} else {
				if o.federation {
					errs = appendError(errs, xmlWrap("FederationTest", func() error {
						return FederationTest(o.testArgs, dump)
					}))
				} else {
					errs = appendError(errs, xmlWrap("Test", func() error {
						return TestWithWrapper(o.testArgs, dump)
					}))
				}
			}
		}
	}

	if o.kubemark {
		errs = appendError(errs, xmlWrap("Kubemark", func() error {
			return KubemarkTest(dump, o)
		}))
	}

	if o.charts {
		errs = appendError(errs, xmlWrap("Helm Charts", ChartsTest))
	}

	if o.perfTests {
		errs = appendError(errs, xmlWrap("Perf Tests", PerfTest))
	}

	if len(errs) > 0 && dump != "" {
		errs = appendError(errs, xmlWrap("DumpClusterLogs", func() error {
			return DumpClusterLogs(dump)
		}))
		if o.federation {
			errs = appendError(errs, xmlWrap("DumpFederationLogs", func() error {
				return DumpFederationLogs(dump)
			}))
		}
	}

	if o.checkLeaks {
		errs = appendError(errs, xmlWrap("ListResources Down", func() error {
			downResources, err = ListResources()
			return err
		}))
	}

	if o.down {
		if o.federation {
			errs = appendError(errs, xmlWrap("Federation TearDown", func() error {
				if !federationDownDone {
					err := FedDown()
					if err != nil {
						return err
					}
					federationDownDone = true
				}
				return nil
			}))
		}
		errs = appendError(errs, xmlWrap("TearDown", func() error {
			if !downDone {
				err := deploy.Down()
				if err != nil {
					return err
				}
				downDone = true
			}
			return nil
		}))
	}

	if o.checkLeaks {
		log.Print("Sleeping for 30 seconds...") // Wait for eventually consistent listing
		time.Sleep(30 * time.Second)
		if err := xmlWrap("ListResources After", func() error {
			afterResources, err = ListResources()
			return err
		}); err != nil {
			errs = append(errs, err)
		} else {
			errs = appendError(errs, xmlWrap("DiffResources", func() error {
				return DiffResources(beforeResources, upResources, downResources, afterResources, dump)
			}))
		}
	}
	if len(errs) == 0 && o.publish != "" {
		errs = appendError(errs, xmlWrap("Publish version", func() error {
			// Use plaintext version file packaged with kubernetes.tar.gz
			if v, err := ioutil.ReadFile("version"); err != nil {
				return err
			} else {
				log.Printf("Set %s version to %s", o.publish, string(v))
			}
			return finishRunning(exec.Command("gsutil", "cp", "version", o.publish))
		}))
	}

	if len(errs) != 0 {
		return fmt.Errorf("encountered %d errors: %v", len(errs), errs)
	}
	return nil
}

func listNodes(dump string) error {
	b, err := output(exec.Command("./cluster/kubectl.sh", "--match-server-version=false", "get", "nodes", "-oyaml"))
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(dump, "nodes.yaml"), b, 0644)
}

func DiffResources(before, clusterUp, clusterDown, after []byte, location string) error {
	if location == "" {
		var err error
		location, err = ioutil.TempDir("", "e2e-check-resources")
		if err != nil {
			return fmt.Errorf("Could not create e2e-check-resources temp dir: %s", err)
		}
	}

	var mode os.FileMode = 0664
	bp := filepath.Join(location, "gcp-resources-before.txt")
	up := filepath.Join(location, "gcp-resources-cluster-up.txt")
	cdp := filepath.Join(location, "gcp-resources-cluster-down.txt")
	ap := filepath.Join(location, "gcp-resources-after.txt")
	dp := filepath.Join(location, "gcp-resources-diff.txt")

	if err := ioutil.WriteFile(bp, before, mode); err != nil {
		return err
	}
	if err := ioutil.WriteFile(up, clusterUp, mode); err != nil {
		return err
	}
	if err := ioutil.WriteFile(cdp, clusterDown, mode); err != nil {
		return err
	}
	if err := ioutil.WriteFile(ap, after, mode); err != nil {
		return err
	}

	stdout, cerr := output(exec.Command("diff", "-sw", "-U0", "-F^\\[.*\\]$", bp, ap))
	if err := ioutil.WriteFile(dp, stdout, mode); err != nil {
		return err
	}
	if cerr == nil { // No diffs
		return nil
	}
	lines := strings.Split(string(stdout), "\n")
	if len(lines) < 3 { // Ignore the +++ and --- header lines
		return nil
	}
	lines = lines[2:]

	var added, report []string
	resourceTypeRE := regexp.MustCompile(`^@@.+\s(\[\s\S+\s\])$`)
	for _, l := range lines {
		if matches := resourceTypeRE.FindStringSubmatch(l); matches != nil {
			report = append(report, matches[1])
		}
		if strings.HasPrefix(l, "+") && len(strings.TrimPrefix(l, "+")) > 0 {
			added = append(added, l)
			report = append(report, l)
		}
	}
	if len(added) > 0 {
		return fmt.Errorf("Error: %d leaked resources\n%v", len(added), strings.Join(report, "\n"))
	}
	return nil
}

func ListResources() ([]byte, error) {
	log.Printf("Listing resources...")
	stdout, err := output(exec.Command("./cluster/gce/list-resources.sh"))
	if err != nil {
		return stdout, fmt.Errorf("Failed to list resources (%s):\n%s", err, string(stdout))
	}
	return stdout, err
}

func clusterSize(deploy deployer) (int, error) {
	if err := deploy.SetupKubecfg(); err != nil {
		return -1, err
	}
	o, err := output(exec.Command("kubectl", "get", "nodes", "--no-headers"))
	if err != nil {
		log.Printf("kubectl get nodes failed: %s\n%s", WrapError(err).Error(), string(o))
		return -1, err
	}
	stdout := strings.TrimSpace(string(o))
	log.Printf("Cluster nodes:\n%s", stdout)
	return len(strings.Split(stdout, "\n")), nil
}

// commandError will provide stderr output (if available) from structured
// exit errors
type commandError struct {
	err error
}

func WrapError(err error) *commandError {
	if err == nil {
		return nil
	}
	return &commandError{err: err}
}

func (e *commandError) Error() string {
	if e == nil {
		return ""
	}
	exitErr, ok := e.err.(*exec.ExitError)
	if !ok {
		return e.err.Error()
	}

	stderr := ""
	if exitErr.Stderr != nil {
		stderr = string(stderr)
	}
	return fmt.Sprintf("%q: %q", exitErr.Error(), stderr)
}

func isUp(d deployer) error {
	n, err := clusterSize(d)
	if err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("cluster found, but %d nodes reported", n)
	}
	return nil
}

func waitForNodes(d deployer, nodes int, timeout time.Duration) error {
	for stop := time.Now().Add(timeout); time.Now().Before(stop); time.Sleep(30 * time.Second) {
		n, err := clusterSize(d)
		if err != nil {
			log.Printf("Can't get cluster size, sleeping: %v", err)
			continue
		}
		if n < nodes {
			log.Printf("%d (current nodes) < %d (requested instances), sleeping", n, nodes)
			continue
		}
		return nil
	}
	return fmt.Errorf("waiting for nodes timed out")
}

func DumpClusterLogs(location string) error {
	log.Printf("Dumping cluster logs to: %v", location)
	return finishRunning(exec.Command("./cluster/log-dump.sh", location))
}

func DumpFederationLogs(location string) error {
	logDumpPath := "./federation/cluster/log-dump.sh"
	// federation/cluster/log-dump.sh only exists in the Kubernetes tree
	// post-1.6. If it doesn't exist, do nothing and do not report an error.
	if _, err := os.Stat(logDumpPath); err == nil {
		log.Printf("Dumping Federation logs to: %v", location)
		return finishRunning(exec.Command(logDumpPath, location))
	}
	log.Printf("Could not find %s. This is expected if running tests against a Kubernetes 1.6 or older tree.", logDumpPath)
	return nil
}

func PerfTest() error {
	// Run perf tests
	// TODO(fejta): GOPATH may be split by :
	cmdline := fmt.Sprintf("%s/src/k8s.io/perf-tests/clusterloader/run-e2e.sh", os.Getenv("GOPATH"))
	if err := finishRunning(exec.Command(cmdline)); err != nil {
		return err
	}
	return nil
}

func ChartsTest() error {
	// Run helm tests.
	if err := finishRunning(exec.Command("/src/k8s.io/charts/test/helm-test-e2e.sh")); err != nil {
		return err
	}
	return nil
}

func KubemarkTest(dump string, o options) error {
	if err := migrateOptions([]migratedOption{
		{
			env:      "KUBEMARK_NUM_NODES",
			option:   &o.kubemarkNodes,
			name:     "--kubemark-nodes",
			skipPush: true,
		},
		{
			env:      "KUBEMARK_MASTER_SIZE",
			option:   &o.kubemarkMasterSize,
			name:     "--kubemark-master-size",
			skipPush: true,
		},
		{
			env:      "KUBEMARK_TEST_ARGS",
			option:   &o.testArgs,
			name:     "--test_args",
			skipPush: true,
		},
	}); err != nil {
		return err
	}

	if dump != "" {
		if pop, err := pushEnv("E2E_REPORT_DIR", dump); err != nil {
			return err
		} else {
			defer pop()
		}
	}
	// Stop previous run
	err := finishRunning(exec.Command("./test/kubemark/stop-kubemark.sh"))
	if err != nil {
		return err
	}
	// If we tried to bring the Kubemark cluster up, make a courtesy
	// attempt to bring it down so we're not leaving resources around.
	//
	// TODO: We should try calling stop-kubemark exactly once. Though to
	// stop the leaking resources for now, we want to be on the safe side
	// and call it explicitly in defer if the other one is not called.
	defer xmlWrap("Deferred Stop kubemark", func() error {
		return finishRunning(exec.Command("./test/kubemark/stop-kubemark.sh"))
	})

	// Start new run
	if popN, err := pushEnv("NUM_NODES", o.kubemarkNodes); err != nil {
		return err
	} else {
		defer popN()
	}
	if popM, err := pushEnv("MASTER_SIZE", o.kubemarkMasterSize); err != nil {
		return err
	} else {
		defer popM()
	}

	err = xmlWrap("Start kubemark", func() error {
		return finishRunning(exec.Command("./test/kubemark/start-kubemark.sh"))
	})
	if err != nil {
		if dump != "" {
			log.Printf("Start kubemark step failed, trying to dump logs from kubemark master...")
			if logErr := finishRunning(exec.Command("./test/kubemark/master-log-dump.sh", dump)); logErr != nil {
				// This can happen in case of non-SSH'able kubemark master.
				log.Printf("Failed to dump logs from kubemark master: %v", logErr)
			}
		}
		return err
	}

	// Run kubemark tests
	focus, present := os.LookupEnv("KUBEMARK_TESTS") // TODO(fejta): migrate to --test_args
	if !present {
		focus = "starting\\s30\\pods"
	}

	err = finishRunning(exec.Command("./test/kubemark/run-e2e-tests.sh", "--ginkgo.focus="+focus, o.testArgs))
	if err != nil {
		return err
	}

	err = xmlWrap("Stop kubemark", func() error {
		return finishRunning(exec.Command("./test/kubemark/stop-kubemark.sh"))
	})
	if err != nil {
		return err
	}
	return nil
}

func UpgradeTest(args, dump string, checkSkew bool) error {
	// TODO(fejta): fix this
	if popS, err := pushd("../kubernetes_skew"); err != nil {
		return err
	} else {
		defer popS()
	}
	if popE, err := pushEnv("E2E_REPORT_PREFIX", "upgrade"); err != nil {
		return err
	} else {
		defer popE()
	}
	return finishRunning(exec.Command(
		kubetestPath,
		"--test",
		"--test_args="+args,
		fmt.Sprintf("--v=%t", verbose),
		fmt.Sprintf("--check-version-skew=%t", checkSkew),
		fmt.Sprintf("--dump=%s", dump),
	))
}

func SkewTest(args, dump string, checkSkew bool) error {
	if popS, err := pushd("../kubernetes_skew"); err != nil {
		return err
	} else {
		defer popS()
	}
	if popE, err := pushEnv("E2E_REPORT_PREFIX", "skew"); err != nil {
		return err
	} else {
		defer popE()
	}
	return finishRunning(exec.Command(
		kubetestPath,
		"--test",
		"--test_args="+args,
		fmt.Sprintf("--v=%t", verbose),
		fmt.Sprintf("--check-version-skew=%t", checkSkew),
		fmt.Sprintf("--dump=%s", dump),
	))
}

func TestWithWrapper(testArgs, dump string) error {
	if dump != "" {
		if pop, err := pushEnv("E2E_REPORT_DIR", dump); err != nil {
			return err
		} else {
			defer pop()
		}
	}
	return finishRunning(exec.Command("./hack/ginkgo-e2e.sh", strings.Fields(testArgs)...))
}

// Direct test using ginkgo / e2e.go rather than ginkgo-e2e.sh
func Test(testArgs, dump string) error {
	ginkgo, err := findKubernetesBinary("ginkgo")
	if err != nil {
		return err
	}
	e2e, err := findKubernetesBinary("e2e.test")
	if err != nil {
		return err
	}

	// TODO(kubernetes/test-infra#3330): Plumb the test options in
	// rather than using magic env variables. This is currently ported
	// almost 1:1 from ginkgo-e2e.sh, but skips everything that
	// scripts shells out for (e.g. KUBE_MASTER_URL must be set before
	// this function will work).
	args := []string{}
	if skip := os.Getenv("CONFORMANCE_TEST_SKIP_REGEX"); skip != "" {
		// Fixed seed for conformance tests.
		// TODO(zmerlynn): Whhhyyyyy?
		args = append(args, "--skip="+skip, "--seed=1436380640")
	}
	if nodes := os.Getenv("GINKGO_PARALLEL_NODES"); nodes != "" {
		args = append(args, "--nodes="+nodes)
	} else if parallel := os.Getenv("GINKGO_PARALLEL"); strings.ToLower(parallel) == "y" {
		args = append(args, "--nodes=25")
	}
	if uif := os.Getenv("GINKGO_UNTIL_IT_FAILS"); uif == "true" {
		args = append(args, "--untilItFails=true")
	}
	if tf := os.Getenv("GINKGO_TOLERATE_FLAKES"); tf == "y" {
		args = append(args, "--ginkgo.flakeAttempts=2")
	} else {
		args = append(args, "--ginkgo.flakeAttempts=1")
	}
	if uif := os.Getenv("GINKGO_NO_COLOR"); uif == "true" {
		args = append(args, "--noColor")
	}

	args = append(args, e2e, "--",
		"--report-dir="+dump,
		"--kubeconfig="+os.Getenv("KUBECONFIG"),
		"--host="+os.Getenv("KUBE_MASTER_URL"),
		"--provider="+os.Getenv("KUBERNETES_PROVIDER"),
		"--gce-project="+os.Getenv("PROJECT"),
		"--gce-zone="+os.Getenv("ZONE"),
		"--gce-multizone="+getenvOrDefault("MULTIZONE", "false"),
		"--gke-cluster="+os.Getenv("CLUSTER_NAME"),
		"--kube-master="+os.Getenv("KUBE_MASTER"),
		"--cluster-tag="+os.Getenv("CLUSTER_ID"),
		"--cloud-config-file="+os.Getenv("CLOUD_CONFIG"),
		"--repo-root="+k8s(),
		"--node-instance-group="+os.Getenv("NODE_INSTANCE_GROUP"),
		"--prefix="+getenvOrDefault("KUBE_GCE_INSTANCE_PREFIX", "e2e"),
		"--network="+getenvOrDefault("KUBE_GCE_NETWORK", getenvOrDefault("KUBE_GKE_NETWORK", "e2e")),
		"--node-tag="+os.Getenv("NODE_TAG"),
		"--master-tag="+os.Getenv("MASTER_TAG"),
		"--federated-kube-context="+getenvOrDefault("FEDERATION_KUBE_CONTEXT", "e2e-federation"))
	if cr := os.Getenv("KUBE_CONTAINER_RUNTIME"); cr != "" {
		args = append(args, "--container-runtime="+cr)
	}
	if mod := os.Getenv("MASTER_OS_DISTRIBUTION"); mod != "" {
		args = append(args, "--master-os-distro="+mod)
	}
	if nod := os.Getenv("NODE_OS_DISTRIBUTION"); nod != "" {
		args = append(args, "--node-os-distro="+nod)
	}
	if nn := os.Getenv("NUM_NODES"); nn != "" {
		args = append(args, "--num-nodes="+nn)
	}
	if cs := os.Getenv("E2E_CLEAN_START"); cs != "" {
		args = append(args, "--clean-start=true")
	}
	if msp := os.Getenv("E2E_MIN_STARTUP_PODS"); msp != "" {
		args = append(args, "--minStartupPods="+msp)
	}
	if rp := os.Getenv("E2E_REPORT_PREFIX"); rp != "" {
		args = append(args, "--report-prefix="+rp)
	}
	args = append(args, strings.Fields(testArgs)...)

	return finishRunning(exec.Command(ginkgo, args...))
}
