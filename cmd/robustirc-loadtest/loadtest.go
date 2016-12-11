package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"k8s.io/client-go/1.5/kubernetes"
	"k8s.io/client-go/1.5/pkg/api"
	apiv1 "k8s.io/client-go/1.5/pkg/api/v1"
	"k8s.io/client-go/1.5/pkg/labels"
	"k8s.io/client-go/1.5/pkg/util/wait"
	"k8s.io/client-go/1.5/tools/clientcmd"

	"github.com/stapelberg/loggedexec"
)

var (
	gcpProjectName = flag.String("gcp_project_name",
		"robustirc-loadtest",
		"Google Cloud Platform project name to use. Used for constructing Google Container Registry paths. Displayed in the top center of e.g. https://console.cloud.google.com/")

	deploymentName = flag.String("gcp_deployment_name",
		"lt",
		"Name of the Container Engine deployment to create, see https://cloud.google.com/deployment-manager/docs/")

	k8sDeploymentName = flag.String("gcp_k8s_deployment_name",
		"rt",
		"Name of the Kubernetes deployment to create, see https://cloud.google.com/deployment-manager/docs/")

	cleanupMode = flag.Bool("cleanup",
		false,
		"Whether to clean up all resources in the Google Cloud Platform project instead of running a load test")
)

// nonZeroExitStatus verifies that err is a loggedexec.ExitError
// with a non-zero exit status. In case it isn’t, err is returned.
func nonZeroExitStatus(err error) bool {
	eerr, ok := err.(*loggedexec.ExitError)
	if !ok {
		// The command could not even be executed. Bail out immediately.
		return false
	}
	stat, ok := eerr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || stat.ExitStatus() == 0 {
		// TODO: when can this happen? is this defense in depth?
		return false
	}
	return true
}

func createDeployment(name, filename string) error {
	cmd := loggedexec.Command("gcloud", "deployment-manager", "deployments", "create", name, "--config", filename)
	cmd.Dir = "deployments"
	err := cmd.Run()
	if err == nil {
		// gcloud blocks until the deployment was actually created,
		// so we are good to go at this point.
		return nil
	}
	if !nonZeroExitStatus(err) {
		return err
	}

	log.Printf("command %v exited with non-zero status, checking if deployment exists", cmd.Args)

	condition := func() (bool, error) {
		// TODO: use loggedexec once Output is supported
		cmd := exec.Command("gcloud", "deployment-manager", "deployments", "describe", name, "--format=json")
		out, err := cmd.Output()
		if err != nil {
			return false, err
		}

		var deploymentStatus struct {
			Deployment struct {
				Operation struct {
					Status string `json:"status"`
				} `json:"operation"`
			} `json:"deployment"`
		}

		if err := json.Unmarshal(out, &deploymentStatus); err != nil {
			return false, err
		}
		got := deploymentStatus.Deployment.Operation.Status
		want := "DONE"
		if got == want {
			log.Printf("Deployment %q in status %q, continuing", name, got)
			return true, nil
		}
		log.Printf("Deployment %q not yet done: got %q, want %q", name, got, want)
		return false, nil
	}

	started := time.Now()
	err = wait.ExponentialBackoff(defaultBackoff, condition)
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("Deployment did not become healthy within %v", time.Since(started))
	}
	return err
}

var defaultBackoff = wait.Backoff{
	Duration: 1 * time.Second,
	Factor:   2,
	Jitter:   0.5, // Wait up to 0.5*duration on each step.
	Steps:    7,   // 7 backoff steps results in a total timeout of about a minute.
}

func waitForPodToSucceed(client *kubernetes.Clientset, podName string, backoff wait.Backoff) error {
	condition := func() (bool, error) {
		pod, err := client.Pods(apiv1.NamespaceDefault).Get(podName)
		if err != nil {
			return false, err
		}
		if pod.Status.Phase == apiv1.PodFailed {
			return true, fmt.Errorf("Pod entered status %v", apiv1.PodFailed)
		}
		if got, want := pod.Status.Phase, apiv1.PodSucceeded; got != want {
			log.Printf("pod %q has not succeeded yet: got %q, want %q", podName, got, want)
			return false, nil
		} else {
			return true, nil
		}
	}

	started := time.Now()
	err := wait.ExponentialBackoff(backoff, condition)
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("pod did not succeed within %v", time.Since(started))
	}
	return err
}

const (
	// Specified in deployment.yaml
	gkeClusterName = "loadtest"
)

func buildContainers(dir, name string) error {
	cmd := loggedexec.Command("make", "container")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return err
	}

	gcrPath := fmt.Sprintf("eu.gcr.io/%s/%s", *gcpProjectName, filepath.Base(name))

	cmd = loggedexec.Command("docker", "tag", name, gcrPath)
	if err := cmd.Run(); err != nil {
		return err
	}

	return loggedexec.Command("gcloud", "docker", "push", gcrPath).Run()
}

func runThroughputBenchmark(client *kubernetes.Clientset) error {
	if _, err := client.Core().Pods(apiv1.NamespaceDefault).Get("throughput"); err == nil {
		log.Printf("deleting pod")
		if err := client.Core().Pods(apiv1.NamespaceDefault).Delete("throughput", &api.DeleteOptions{}); err != nil {
			return err
		}
	}

	if _, err := client.Core().Services(apiv1.NamespaceDefault).Get("throughput"); err == nil {
		log.Printf("deleting service")
		if err := client.Core().Services(apiv1.NamespaceDefault).Delete("throughput", &api.DeleteOptions{}); err != nil {
			return err
		}
	}

	// Wait for the pod to actually be deleted, as per
	// https://github.com/kubernetes/kubernetes/issues/28115
	condition := func() (bool, error) {
		_, err := client.Core().Pods(apiv1.NamespaceDefault).Get("throughput")
		return (err != nil), nil
	}

	started := time.Now()
	if err := wait.ExponentialBackoff(defaultBackoff, condition); err != nil {
		if err == wait.ErrWaitTimeout {
			return fmt.Errorf("Pod was not deleted within %v", time.Since(started))
		}
		return err
	}

	// TODO: replace robustirc-loadtest with *gcpProjectName
	cmd := loggedexec.Command("kubectl", "create", "-f", "deployments/throughput.pod.yaml")
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = loggedexec.Command("kubectl", "create", "-f", "deployments/throughput.svc.yaml")
	return cmd.Run()
}

func getExternalIPs() (map[string]string, error) {
	cmd := exec.Command("gcloud", "compute", "instances", "list", "--format=json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var instances []struct {
		Name              string `json:"name"`
		NetworkInterfaces []struct {
			AccessConfigs []struct {
				NatIP string `json:"natIP"`
			} `json:"accessConfigs"`
		} `json:"networkInterfaces"`
	}

	if err := json.Unmarshal(out, &instances); err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, instance := range instances {
		if len(instance.NetworkInterfaces) < 1 || len(instance.NetworkInterfaces[0].AccessConfigs) < 1 {
			log.Printf("Skipping instance %+v (expected NetworkInterfaces[0].AccessConfigs[0] field not found)", instance)
			continue
		}
		result[instance.Name] = instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
	}
	return result, nil
}

func createFirewallRule() error {
	cmd := loggedexec.Command("gcloud", "compute", "firewall-rules", "create",
		"allow-gke-nodeports",
		"--allow=tcp:30000-32767",
		"--description=See http://kubernetes.io/docs/user-guide/services/#type-nodeport",
		"--network=default",
		"--source-ranges=0.0.0.0/0")
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if !nonZeroExitStatus(err) {
		return err
	}

	log.Printf("command %v exited with non-zero status, checking if firewall rule exists", cmd.Args)

	return loggedexec.Command("gcloud", "compute", "firewall-rules", "describe", "allow-gke-nodeports").Run()
}

// TODO: add a flag to disable restarting the network so that we can
// run repeated benchmarks. messages seem to drop significantly when
// doing that, not yet sure why
func restartNetwork(client *kubernetes.Clientset) error {
	// Set replicas==0 on all Replication Controllers with
	// app=robustirc-node to delete all RobustIRC node pods.
	replicationControllers, err := client.Core().ReplicationControllers(apiv1.NamespaceDefault).List(api.ListOptions{
		LabelSelector: labels.Set{"app": "robustirc-node"}.AsSelector(),
	})
	if err != nil {
		return err
	}
	if len(replicationControllers.Items) == 0 {
		return nil
	}
	log.Printf("Restarting Replication Controllers with app=robustirc-node label")
	for _, rc := range replicationControllers.Items {
		replicas := int32(0)
		rc.Spec.Replicas = &replicas
		if _, err := client.Core().ReplicationControllers(apiv1.NamespaceDefault).Update(&rc); err != nil {
			return err
		}
	}

	// Wait until existingPods returns 0 pods, to be sure we get a new network.
	condition := func() (bool, error) {
		existingPods, err := client.Core().Pods(apiv1.NamespaceDefault).List(api.ListOptions{
			LabelSelector: labels.Set{"app": "robustirc-node"}.AsSelector(),
		})
		return len(existingPods.Items) == 0, err
	}

	started := time.Now()
	if err := wait.ExponentialBackoff(defaultBackoff, condition); err != nil {
		if err == wait.ErrWaitTimeout {
			return fmt.Errorf("Pods were not deleted within %v", time.Since(started))
		}
		return err
	}

	// Bring the network back up.
	replicationControllers, err = client.Core().ReplicationControllers(apiv1.NamespaceDefault).List(api.ListOptions{
		LabelSelector: labels.Set{"app": "robustirc-node"}.AsSelector(),
	})
	if err != nil {
		return err
	}
	for _, rc := range replicationControllers.Items {
		replicas := int32(1)
		rc.Spec.Replicas = &replicas
		if _, err := client.Core().ReplicationControllers(apiv1.NamespaceDefault).Update(&rc); err != nil {
			return err
		}
	}
	return nil
}

// loadtest contains the logic to set up and run a load test.
// This logic is encapsulated into a separate function so that we can return an
// error (which main() passes to log.Fatal), yet still have our defer
// statements evaluated (which would not happen when directly calling
// log.Fatal).
func loadtest() error {
	requiredPaths := []string{
		"Dockerfile",
		"../robustirc/Dockerfile",
		"../bridge/Dockerfile",

		"deployments/cluster.yaml",
		"deployments/kubernetes.yaml",
		"deployments/throughput.pod.yaml",
		"deployments/throughput.svc.yaml",

		"../robustirc/contrib/grafana/robustirc.json",
		"../robustirc/contrib/grafana/robustirc_loadtest.json",
	}

	for _, path := range requiredPaths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("Required file %q not found. Are you running this loadtest from $GOPATH/src/github.com/robustirc/benchmark?", path)
		}
	}

	// Start building and pushing docker containers in parallel and asynchronously.
	var buildWg errgroup.Group
	buildWg.Go(func() error { return buildContainers(".", "robustirc/benchmark") })
	buildWg.Go(func() error { return buildContainers("../robustirc", "robustirc/robustirc") })
	buildWg.Go(func() error { return buildContainers("../bridge", "robustirc/bridge") })

	if err := createFirewallRule(); err != nil {
		return err
	}

	if err := createDeployment(*deploymentName, "cluster.yaml"); err != nil {
		return err
	}

	// TODO(later): make robustirc nodes available on external IPs, so that we can do pprof profiling
	// // TODO: can be done in parallel with the following createDeployment
	// ips, err := getExternalIPs()
	// if err != nil {
	// 	return err
	// }

	// log.Printf("external IPs = %+v", ips)

	if err := loggedexec.Command("gcloud", "container", "clusters", "get-credentials", gkeClusterName).Run(); err != nil {
		return err
	}

	kConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{})
	kubeConfig, err := kConfig.ClientConfig()
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	if err := buildWg.Wait(); err != nil {
		return err
	}

	if err := restartNetwork(kubeClient); err != nil {
		return err
	}

	if err := createDeployment(*k8sDeploymentName, "kubernetes.yaml"); err != nil {
		return err
	}

	if err := runThroughputBenchmark(kubeClient); err != nil {
		return err
	}

	throughputBackoff := wait.Backoff{
		Duration: 1 * time.Second,
		Factor:   2,
		Jitter:   0.5, // Wait up to 0.5*duration on each step.
		Steps:    9,   // 7 backoff steps results in a total timeout of about a minute.
	}

	if err := waitForPodToSucceed(kubeClient, "throughput", throughputBackoff); err != nil {
		return err
	}

	body, err := kubeClient.Core().Pods(apiv1.NamespaceDefault).GetLogs("throughput", &apiv1.PodLogOptions{}).Do().Raw()
	if err != nil {
		return err
	}
	log.Printf("body = %s", string(body))

	return nil
}

func deleteDeployment(name string) error {
	condition := func() (bool, error) {
		// TODO: use loggedexec once Output is supported
		cmd := loggedexec.Command("gcloud", "deployment-manager", "deployments", "delete", name)
		cmd.Stdin = strings.NewReader("y")
		if err := cmd.Run(); !nonZeroExitStatus(err) {
			return false, err
		}

		// verify the deployment does not exist
		var stderr bytes.Buffer
		vcmd := exec.Command("gcloud", "deployment-manager", "deployments", "describe", name)
		vcmd.Stderr = &stderr
		err := vcmd.Run()
		if err == nil {
			// Not done: the deployment still exists
			return false, nil
		}
		// gcloud prints a message like the following to stderr:
		// ERROR: (gcloud.deployment-manager.deployments.describe) ResponseError: code=404, message=The object 'projects/robustirc-loadtest/global/deployments/bleh' is not found.
		// XXX: find a better (more robust) API to do the same. Is there a Go library for the deployment manager?
		if strings.Contains(stderr.String(), "code=404") {
			return true, nil
		}
		return false, err
	}

	started := time.Now()
	err := wait.ExponentialBackoff(defaultBackoff, condition)
	if err == wait.ErrWaitTimeout {
		return fmt.Errorf("Deployment could not be deleted within %v", time.Since(started))
	}
	return err
}

func deleteManifest(baseUrl, image, manifest string) error {
	url := baseUrl + image + "/manifests/" + manifest
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("%q: Unexpected HTTP status code: got %d, want %d", url, got, want)
	}
	return nil
}

// See also https://stackoverflow.com/questions/31523945/how-to-remove-a-pushed-image-in-google-container-registry?rq=1
func deleteImage(baseUrl, image string) error {
	resp, err := http.Get(baseUrl + image + "/tags/list")
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("Unexpected HTTP status code: got %d, want %d", got, want)
	}
	var tagsList struct {
		Manifest map[string]struct{} `json:"manifest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsList); err != nil {
		return err
	}

	// Remove the “latest” tag first, otherwise the manifest which has
	// the “latest” tag cannot be deleted.
	if err := deleteManifest(baseUrl, image, "latest"); err != nil {
		return err
	}

	var wg errgroup.Group
	for reference, _ := range tagsList.Manifest {
		// Copy reference into the local scope so that it is captured
		// (instead of overwritten) in the goroutine below.
		reference := reference
		wg.Go(func() error { return deleteManifest(baseUrl, image, reference) })
	}

	return wg.Wait()
}

func cleanup() error {
	// The kubernetes deployment needs to be deleted explicitly. While
	// it logically depends on the cluster deployment, deleting the
	// cluster deployment is not enough — the kubernetes deployment
	// would stick around and block future load tests.
	if err := deleteDeployment("rt"); err != nil {
		return err
	}

	if err := deleteDeployment("lt"); err != nil {
		return err
	}

	token, err := exec.Command("gcloud", "auth", "print-access-token").Output()
	if err != nil {
		return err
	}
	baseUrl := fmt.Sprintf("https://_token:%s@eu.gcr.io/v2/%s/", strings.TrimSpace(string(token)), *gcpProjectName)
	for _, image := range []string{"robustirc", "benchmark", "bridge"} {
		if err := deleteImage(baseUrl, image); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	flag.Parse()

	if *cleanupMode {
		if err := cleanup(); err != nil {
			log.Fatal(err)
		}
		return
	}

	// TODO: figure out the inputs to this, i.e. possibly a hash of the config, and a hash of the binary (distributed via docker), compile a report
	log.Printf("running loadtest\n")

	if err := loadtest(); err != nil {
		log.Fatal(err)
	}
}
