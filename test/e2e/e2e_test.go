//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/USA-RedDragon/b2-backup-operator/test/utils"
)

const namespace = "b2-backup-operator-system"

const serviceAccountName = "b2-backup-operator-controller-manager"

const metricsServiceName = "b2-backup-operator-controller-manager-metrics-service"

const metricsRoleBindingName = "b2-backup-operator-metrics-binding"

const curlImage = "curlimages/curl:8.22.0"

const conflictMessage = "the object has been modified"

const unrecognizedOperation = "unrecognized GraphQL operation"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("deploying the BorgBase stub and its API token")
		cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/borgbase-stub.yaml")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the BorgBase stub")

		By("pointing the operator at the stub")
		cmd = exec.Command("kubectl", "patch", "deployment", "b2-backup-operator-controller-manager",
			"-n", namespace, "--type=json", "-p",
			`[{"op":"add","path":"/spec/template/spec/containers/0/args/-",`+
				`"value":"--borgbase-endpoint=http://borgbase-stub.`+namespace+`.svc:8080/graphql"}]`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to point the operator at the stub")

		By("waiting for the controller-manager to roll out")
		cmd = exec.Command("kubectl", "rollout", "status",
			"deployment/b2-backup-operator-controller-manager", "-n", namespace, "--timeout=3m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "controller-manager did not roll out")

		By("recording the name of the controller-manager pod")
		Eventually(func(g Gomega) {
			podNames := controllerPodNames(g)
			g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
			controllerPodName = podNames[0]
		}).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			if controllerPodName == "" {
				_, _ = fmt.Fprintf(GinkgoWriter, "No controller-manager pod recorded; skipping its logs\n")
			} else {
				By("Fetching controller manager pod logs")
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				controllerLogs, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
				}
			}

			By("Fetching Kubernetes events")
			cmd := exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			if controllerPodName != "" {
				By("Fetching controller manager pod description")
				cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
				podDescription, err := utils.Run(cmd)
				if err == nil {
					fmt.Println("Pod description:\n", podDescription)
				} else {
					fmt.Println("Failed to describe controller pod")
				}
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				podNames := controllerPodNames(g)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd := exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=b2-backup-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")
			DeferCleanup(func() {
				By("removing the metrics ClusterRoleBinding")
				_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding",
					metricsRoleBindingName, "--ignore-not-found"))
			})

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			curlCmd := fmt.Sprintf("for i in $(seq 1 30); do curl -sS -k -H 'Authorization: Bearer %s' "+
				"-w '\\nHTTP_STATUS:%%{http_code}\\n' "+
				"https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1",
				token, metricsServiceName, namespace)
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image="+curlImage,
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": %q,
							"command": ["/bin/sh", "-c"],
							"args": [%q],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, curlImage, curlCmd, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("HTTP_STATUS:200"),
					"the metrics endpoint did not answer 200")
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		It("should refuse to schedule a backup with no repository", func() {
			const (
				testNS = "b2backup-e2e"
				name   = "orphaned"
			)

			By("creating a test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the test namespace")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found"))
			})

			By("applying a ScheduledBackup that references a missing Repository")
			manifest := fmt.Sprintf(`apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: %s
  namespace: %s
spec:
  repositoryRef:
    name: does-not-exist
  schedule: "@hourly"
  sources:
    - type: cnpg
      tag: db
  healthchecks:
    enabled: false
`, name, testNS)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply the ScheduledBackup")

			By("waiting for the controller to report the missing repository")
			verifyNotReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "scheduledbackup", name, "-n", testNS,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].reason}")
				reason, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("RepositoryNotFound"))
			}
			Eventually(verifyNotReady, 2*time.Minute).Should(Succeed())

			By("verifying no CronJob was created")
			cmd = exec.Command("kubectl", "get", "cronjob", "-n", testNS,
				"-o", "jsonpath={.items[*].metadata.name}")
			cronjobs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(cronjobs).To(BeEmpty(), "a backup was scheduled against a missing repository")
		})

		It("should create a repository and write its credentials", func() {
			const (
				testNS = "b2backup-e2e-repo"
				name   = "restic"
			)

			By("creating a test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the test namespace")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found"))
			})

			By("applying a Repository")
			manifest := fmt.Sprintf(`apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: %s
  namespace: %s
spec:
  region: us
  quotaGiB: 100
`, name, testNS)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply the Repository")

			By("waiting for the repository to be recorded in status")
			var repositoryID string
			verifyRecorded := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", name, "-n", testNS,
					"-o", "jsonpath={.status.repositoryID}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty(), "no repository ID was recorded")
				repositoryID = strings.TrimSpace(out)
			}
			Eventually(verifyRecorded, 2*time.Minute).Should(Succeed())

			By("checking the credentials Secret")

			cmd = exec.Command("kubectl", "get", "secret", name+"-b2", "-n", testNS,
				"-o", "jsonpath={.data.RESTIC_PASSWORD}")
			password, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "no credentials Secret was written")
			Expect(password).NotTo(BeEmpty(), "the Secret holds no password")

			cmd = exec.Command("kubectl", "get", "secret", name+"-b2", "-n", testNS,
				"-o", "jsonpath={.data.RESTIC_REPOSITORY}")
			encoded, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			url, err := base64.StdEncoding.DecodeString(encoded)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(url)).To(ContainSubstring(repositoryID + ".repo.borgbase.com"))

			By("checking the Secret is not owned under the default Retain policy")
			cmd = exec.Command("kubectl", "get", "secret", name+"-b2", "-n", testNS,
				"-o", "jsonpath={.metadata.ownerReferences}")
			owners, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(owners).To(BeEmpty(),
				"Retain must not own the Secret; it is the only copy of the encryption key")

			By("waiting for the init Job to be created")

			verifyInitJob := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", name+"-init", "-n", testNS,
					"-o", "jsonpath={.metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal(name + "-init"))
			}
			Eventually(verifyInitJob, 2*time.Minute).Should(Succeed())

			By("checking the Initialized condition agrees with the init Job")
			verifyInitializing := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", name+"-init", "-n", testNS,
					"-o", "jsonpath={.status.conditions[?(@.type=='Failed')].status}")
				jobFailed, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				cmd = exec.Command("kubectl", "get", "repository", name, "-n", testNS,
					"-o", "jsonpath={.status.conditions[?(@.type=='Initialized')].reason}")
				reason, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				if strings.TrimSpace(jobFailed) == "True" {
					g.Expect(reason).To(Equal("InitFailed"),
						"the init Job failed but the Repository does not say so")
					return
				}
				g.Expect(reason).To(Equal("Initializing"),
					"the init Job has not failed but the Repository is not initializing")
			}
			Eventually(verifyInitializing, 2*time.Minute).Should(Succeed())
		})

		It("should refuse to adopt a repository that does not exist", func() {
			const (
				testNS = "b2backup-e2e-adopt"
				name   = "restic"
			)

			By("creating a test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the test namespace")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found"))
			})

			By("adopting an ID the stub has never heard of")
			manifest := fmt.Sprintf(`apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: %s
  namespace: %s
spec:
  existingRepositoryID: deadbeef
  passwordSecretRef:
    name: restic-envs
    key: RESTIC_PASSWORD
`, name, testNS)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply the Repository")

			By("waiting for it to report the failure")
			verifyFailed := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", name, "-n", testNS,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				status, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("False"))
			}
			Eventually(verifyFailed, 2*time.Minute).Should(Succeed())

			By("verifying no repository was created and no Secret written")
			cmd = exec.Command("kubectl", "get", "repository", name, "-n", testNS,
				"-o", "jsonpath={.status.repositoryID}")
			id, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(id).To(BeEmpty(), "a repository was provisioned for a bad adoption ID")

			cmd = exec.Command("kubectl", "get", "secret", name+"-b2", "-n", testNS)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "credentials were written for a repository that does not exist")
		})

		It("should persist status without conflicting on its own writes", func() {
			const (
				testNS = "b2backup-e2e-status"
				name   = "churn"
			)

			specStart := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)

			By("creating a test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the test namespace")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found"))
			})

			manifest := fmt.Sprintf(`apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: %s
  namespace: %s
spec:
  repositoryRef:
    name: does-not-exist
  schedule: "@hourly"
  sources:
    - type: cnpg
      tag: db
  healthchecks:
    enabled: false
`, name, testNS)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply the ScheduledBackup")

			By("forcing repeated reconciles")

			for i := range 5 {
				cmd = exec.Command("kubectl", "patch", "scheduledbackup", name, "-n", testNS,
					"--type=merge", "-p", fmt.Sprintf(`{"spec":{"timeZone":"Etc/GMT+%d"}}`, i+1))
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to patch the ScheduledBackup")
			}

			By("waiting for status to catch up with the latest generation")
			verifyObserved := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "scheduledbackup", name, "-n", testNS,
					"-o", "jsonpath={.metadata.generation},{.status.observedGeneration}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				parts := strings.Split(strings.TrimSpace(out), ",")
				g.Expect(parts).To(HaveLen(2))
				g.Expect(parts[1]).To(Equal(parts[0]),
					"observedGeneration never caught up, so a status write was lost")
			}
			Eventually(verifyObserved, 2*time.Minute).Should(Succeed())

			By("verifying the controller logged no resourceVersion conflicts on this object")
			cmd = exec.Command("kubectl", "logs", controllerPodName, "-n", namespace,
				"--since-time="+specStart)
			logs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to read controller logs")
			for _, line := range utils.GetNonEmptyLines(logs) {
				if !strings.Contains(line, conflictMessage) {
					continue
				}
				Expect(line).NotTo(ContainSubstring(name),
					"the controller conflicted with its own writes")
			}
		})

		It("should list what the operator manages through b2bk", func() {
			const (
				testNS = "b2backup-e2e-b2bk"
				name   = "listed"
			)

			By("creating a test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the test namespace")
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found"))
			})

			By("applying a Repository")
			manifest := fmt.Sprintf(`apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: %s
  namespace: %s
spec:
  region: us
  quotaGiB: 100
`, name, testNS)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply the Repository")

			By("waiting for the repository to be recorded in status")
			var repositoryID string
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "repository", name, "-n", testNS,
					"-o", "jsonpath={.status.repositoryID}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty(), "no repository ID was recorded")
				repositoryID = strings.TrimSpace(out)
			}, 2*time.Minute).Should(Succeed())

			By("listing repositories with b2bk")
			cmd = exec.Command("go", "run", "./cmd/kubectl-b2bk",
				"get", "repositories", "-n", testNS, "-o", "json")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "b2bk get failed")
			Expect(out).To(ContainSubstring(`"kind": "RepositoryList"`))
			Expect(out).To(ContainSubstring(repositoryID),
				"b2bk did not report the repository the API server holds")
		})

		It("should not have sent the BorgBase stub an operation it did not recognize", func() {
			By("reading the BorgBase stub logs")
			cmd := exec.Command("kubectl", "logs", "-n", namespace,
				"-l", "app.kubernetes.io/name=borgbase-stub", "--tail=-1")
			logs, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to read the BorgBase stub logs")
			Expect(logs).NotTo(ContainSubstring(unrecognizedOperation),
				"the stub did not recognize a query from internal/borgbase; "+
					"teach test/e2e/stub/main.go about it rather than relaxing this")
		})
	})
})

func controllerPodNames(g Gomega) []string {
	cmd := exec.Command("kubectl", "get",
		"pods", "-l", "control-plane=controller-manager",
		"-o", "go-template={{ range .items }}"+
			"{{ if not .metadata.deletionTimestamp }}"+
			"{{ .metadata.name }}"+
			"{{ \"\\n\" }}{{ end }}{{ end }}",
		"-n", namespace,
	)
	podOutput, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
	return utils.GetNonEmptyLines(podOutput)
}

func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
