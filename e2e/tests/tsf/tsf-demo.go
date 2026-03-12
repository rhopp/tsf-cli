package tsf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	buildcontrollers "github.com/konflux-ci/build-service/controllers"
	tektonutils "github.com/konflux-ci/release-service/tekton/utils"

	"github.com/devfile/library/v2/pkg/util"
	"github.com/google/go-github/v44/github"
	appservice "github.com/konflux-ci/application-api/api/v1alpha1"
	"github.com/konflux-ci/e2e-tests/pkg/clients/has"
	"github.com/konflux-ci/e2e-tests/pkg/constants"
	"github.com/konflux-ci/e2e-tests/pkg/framework"
	"github.com/konflux-ci/e2e-tests/pkg/utils"
	"github.com/konflux-ci/e2e-tests/pkg/utils/build"
	"github.com/konflux-ci/e2e-tests/pkg/utils/tekton"
	imagecontrollerv1alpha1 "github.com/konflux-ci/image-controller/api/v1alpha1"
	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	integrationv1beta2 "github.com/konflux-ci/integration-service/api/v1beta2"
	releaseApi "github.com/konflux-ci/release-service/api/v1alpha1"
	releaseMetadata "github.com/konflux-ci/release-service/metadata"
	tektonapi "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	tsfTestLabel = "tsf-demo"

	// Timeouts
	mergePRTimeout              = time.Minute * 1
	pipelineRunStartedTimeout   = time.Minute * 5
	pullRequestCreationTimeout  = time.Minute * 5
	snapshotTimeout             = time.Minute * 4
	releaseTimeout              = time.Minute * 4
	releasePipelineTimeout      = time.Minute * 15
	imageRepositoryReadyTimeout = time.Minute * 5
	customResourceUpdateTimeout = time.Minute * 10

	// Intervals
	defaultPollingInterval  = time.Second * 2
	snapshotPollingInterval = time.Second * 1
	releasePollingInterval  = time.Second * 1

	// Release configuration (following setup-release.sh pattern)
	releasePipelineSAName         = "release-pipeline"
	ecPolicySourceNS              = "enterprise-contract-service"
	ecPolicySourceName            = "default"
	releaseServiceCatalogURL      = "https://github.com/konflux-ci/release-service-catalog.git"
	releaseServiceCatalogRevision = "production"
	releasePipelinePath           = "pipelines/managed/push-to-external-registry/push-to-external-registry.yaml"
)

func tsfDemoSuiteDescribe(args ...interface{}) bool {
	return ginkgo.Describe("[tsf-demo-suite]", args)
}

var _ = tsfDemoSuiteDescribe(ginkgo.Label(tsfTestLabel), func() {
	defer ginkgo.GinkgoRecover()

	var userNamespace string
	var err error

	fw := &framework.Framework{}

	var applicationName string
	var component *appservice.Component
	var componentNewBaseBranch, gitRevision, componentRepositoryName, componentName string
	var buildPipelineAnnotation map[string]string

	var managedNamespace string
	var pipelineRun, testPipelinerun *tektonapi.PipelineRun
	var snapshot *appservice.Snapshot
	var integrationTestScenario *integrationv1beta2.IntegrationTestScenario
	var release *releaseApi.Release

	// PaC related variables
	var prNumber int
	var headSHA, pacBranchName string
	var mergeResult *github.PullRequestMergeResult

	// Component configuration - using a simple test repository
	var gitSourceUrl string
	const (
		gitSourceRevision          = "1255dc36534b9db7b99efbc281117435ea03255f"
		gitSourceDefaultBranchName = "main"
		dockerFilePath             = "Dockerfile"

		// Integration test scenario
		itsGitURL      = "https://github.com/konflux-ci/build-definitions"
		itsGitRevision = "main"
		itsTestPath    = "pipelines/enterprise-contract.yaml"
	)

	ginkgo.Describe("TSF Demo", ginkgo.Ordered, func() {
		ginkgo.BeforeAll(func() {
			if os.Getenv(constants.SKIP_PAC_TESTS_ENV) == "true" {
				ginkgo.Skip("Skipping this test due to configuration issue with Spray proxy")
			}

			githubOrg := os.Getenv("MY_GITHUB_ORG")
			gomega.Expect(githubOrg).NotTo(gomega.BeEmpty(), "MY_GITHUB_ORG env var is not set")
			gitSourceUrl = fmt.Sprintf("https://github.com/%s/testrepo", githubOrg)

			fw, err = framework.NewFramework(utils.GetGeneratedNamespace(tsfTestLabel))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			userNamespace = fw.UserNamespace
			managedNamespace = userNamespace + "-managed"

			// Application config
			applicationName = fmt.Sprintf("tsf-app-%s", util.GenerateRandomString(4))

			// Component config
			componentName = fmt.Sprintf("tsf-comp-%s", util.GenerateRandomString(4))
			pacBranchName = fmt.Sprintf("%s%s", constants.PaCPullRequestBranchPrefix, componentName)
			componentRepositoryName = utils.ExtractGitRepositoryNameFromURL(gitSourceUrl)

			// Get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuildOciTA)

			// Set up release infrastructure in managed namespace
			createTsfReleaseConfig(fw, managedNamespace, userNamespace, applicationName, componentName)
		})

		// Remove all resources created by the tests
		ginkgo.AfterAll(func() {
			if !(os.Getenv("E2E_SKIP_CLEANUP") == "true") && !ginkgo.CurrentSpecReport().Failed() {
				gomega.Expect(fw.AsKubeAdmin.ReleaseController.DeleteReleasePlan("tsf-release", userNamespace, false)).To(gomega.Succeed())
				gomega.Expect(fw.AsKubeAdmin.HasController.DeleteApplication(applicationName, userNamespace, false)).To(gomega.Succeed())
				gomega.Expect(fw.AsKubeAdmin.CommonController.DeleteNamespace(managedNamespace)).To(gomega.Succeed())

				// Delete new branch created by PaC and a testing branch used as a component's base branch
				err = fw.AsKubeAdmin.CommonController.Github.DeleteRef(componentRepositoryName, pacBranchName)
				if err != nil {
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("Reference does not exist"))
				}
				err = fw.AsKubeAdmin.CommonController.Github.DeleteRef(componentRepositoryName, componentNewBaseBranch)
				if err != nil {
					gomega.Expect(err.Error()).To(gomega.ContainSubstring("Reference does not exist"))
				}
				gomega.Expect(build.CleanupWebhooks(fw, componentRepositoryName)).ShouldNot(gomega.HaveOccurred())
			}
		})

		// Create an application in a specific namespace
		ginkgo.It("creates an application", ginkgo.Label(tsfTestLabel), func() {
			createdApplication, err := fw.AsKubeAdmin.HasController.CreateApplication(applicationName, userNamespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(createdApplication.Spec.DisplayName).To(gomega.Equal(applicationName))
			gomega.Expect(createdApplication.Namespace).To(gomega.Equal(userNamespace))
		})

		// Create an IntegrationTestScenario for the App
		ginkgo.It("creates an IntegrationTestScenario for the app", ginkgo.Label(tsfTestLabel), func() {
			gomega.Eventually(func() error {
				var err error
				integrationTestScenario, err = fw.AsKubeAdmin.IntegrationController.CreateIntegrationTestScenario("", applicationName, userNamespace, itsGitURL, itsGitRevision, itsTestPath, "", []string{})
				return err
			}, time.Minute*2, time.Second*5).Should(gomega.Succeed())
		})

		ginkgo.It("creates new branch for the build", ginkgo.Label(tsfTestLabel), func() {
			// We need to create a new branch that we will target
			// and that will contain the PaC configuration, so we
			// can avoid polluting the default (main) branch
			componentNewBaseBranch = fmt.Sprintf("base-%s", util.GenerateRandomString(6))
			gitRevision = componentNewBaseBranch
			gomega.Expect(fw.AsKubeAdmin.CommonController.Github.CreateRef(componentRepositoryName, gitSourceDefaultBranchName, gitSourceRevision, componentNewBaseBranch)).To(gomega.Succeed())
		})

		// Component is imported from gitUrl
		ginkgo.It(fmt.Sprintf("creates component %s from git source %s", componentName, gitSourceUrl), ginkgo.Label(tsfTestLabel), func() {
			componentObj := appservice.ComponentSpec{
				ComponentName: componentName,
				Application:   applicationName,
				Source: appservice.ComponentSource{
					ComponentSourceUnion: appservice.ComponentSourceUnion{
						GitSource: &appservice.GitSource{
							URL:           gitSourceUrl,
							Revision:      gitRevision,
							DockerfileURL: dockerFilePath,
						},
					},
				},
			}

			component, err = fw.AsKubeAdmin.HasController.CreateComponentCheckImageRepository(componentObj, userNamespace, "", "", applicationName, false, utils.MergeMaps(constants.ComponentPaCRequestAnnotation, buildPipelineAnnotation))
			gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
		})

		ginkgo.When("Component is created", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("triggers creation of a PR in the sample repo", func() {
				var prSHA string
				gomega.Eventually(func() error {
					prs, err := fw.AsKubeAdmin.CommonController.Github.ListPullRequests(componentRepositoryName)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					for _, pr := range prs {
						if pr.Head.GetRef() == pacBranchName {
							prNumber = pr.GetNumber()
							prSHA = pr.GetHead().GetSHA()
							return nil
						}
					}
					return fmt.Errorf("could not get the expected PaC branch name %s", pacBranchName)
				}, pullRequestCreationTimeout, defaultPollingInterval).Should(gomega.Succeed(), fmt.Sprintf("timed out when waiting for init PaC PR (branch %q) to be created against the %q repo", pacBranchName, componentRepositoryName))

				// We don't need the PipelineRun from a PaC 'pull-request' event to finish, so we can delete it
				gomega.Eventually(func() error {
					pipelineRun, err = fw.AsKubeAdmin.HasController.GetComponentPipelineRun(component.GetName(), applicationName, userNamespace, prSHA)
					if err == nil {
						gomega.Expect(fw.AsKubeAdmin.TektonController.DeletePipelineRun(pipelineRun.Name, pipelineRun.Namespace)).To(gomega.Succeed())
						return nil
					}
					return err
				}, pipelineRunStartedTimeout, constants.PipelineRunPollingInterval).Should(gomega.Succeed(), fmt.Sprintf("timed out when waiting for `pull-request` event type PaC PipelineRun to be present in the user namespace %q for component %q with a label pointing to %q", userNamespace, component.GetName(), applicationName))
			})

			ginkgo.It("verifies component build status", func() {
				var buildStatus *buildcontrollers.BuildStatus
				gomega.Eventually(func() (bool, error) {
					component, err := fw.AsKubeAdmin.HasController.GetComponent(component.GetName(), userNamespace)
					if err != nil {
						return false, err
					}

					statusBytes := []byte(component.Annotations[buildcontrollers.BuildStatusAnnotationName])

					err = json.Unmarshal(statusBytes, &buildStatus)
					if err != nil {
						return false, err
					}

					if buildStatus.PaC != nil {
						ginkgo.GinkgoWriter.Printf("state: %s\n", buildStatus.PaC.State)
						ginkgo.GinkgoWriter.Printf("mergeUrl: %s\n", buildStatus.PaC.MergeUrl)
						ginkgo.GinkgoWriter.Printf("errId: %d\n", buildStatus.PaC.ErrId)
						ginkgo.GinkgoWriter.Printf("errMessage: %s\n", buildStatus.PaC.ErrMessage)
						ginkgo.GinkgoWriter.Printf("configurationTime: %s\n", buildStatus.PaC.ConfigurationTime)
					} else {
						ginkgo.GinkgoWriter.Println("build status does not have PaC field")
					}

					return buildStatus.PaC != nil && buildStatus.PaC.State == "enabled" && buildStatus.PaC.MergeUrl != "" && buildStatus.PaC.ErrId == 0 && buildStatus.PaC.ConfigurationTime != "", nil
				}, pipelineRunStartedTimeout, defaultPollingInterval).Should(gomega.BeTrue(), "component build status has unexpected content")
			})

			ginkgo.It("should eventually lead to triggering a 'push' event type PipelineRun after merging the PaC init branch ", func() {
				gomega.Eventually(func() error {
					mergeResult, err = fw.AsKubeAdmin.CommonController.Github.MergePullRequest(componentRepositoryName, prNumber)
					return err
				}, mergePRTimeout).ShouldNot(gomega.HaveOccurred(), fmt.Sprintf("error when merging PaC pull request: %+v\n", err))

				headSHA = mergeResult.GetSHA()

				gomega.Eventually(func() error {
					pipelineRun, err = fw.AsKubeAdmin.HasController.GetComponentPipelineRun(component.GetName(), applicationName, userNamespace, headSHA)
					if err != nil {
						ginkgo.GinkgoWriter.Printf("PipelineRun has not been created yet for component %s/%s\n", userNamespace, component.GetName())
						return err
					}
					if !pipelineRun.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", pipelineRun.GetNamespace(), pipelineRun.GetName())
					}
					return nil
				}, pipelineRunStartedTimeout, constants.PipelineRunPollingInterval).Should(gomega.Succeed(), fmt.Sprintf("timed out when waiting for a PipelineRun in namespace %q with label component label %q and application label %q and sha label %q to start", userNamespace, component.GetName(), applicationName, headSHA))
			})
		})

		ginkgo.When("Build PipelineRun is created", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("does not contain an annotation with a Snapshot Name", func() {
				gomega.Expect(pipelineRun.Annotations["appstudio.openshift.io/snapshot"]).To(gomega.Equal(""))
			})
			ginkgo.It("should eventually complete successfully", func() {
				gomega.Expect(fw.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(component, "build", headSHA, "",
					fw.AsKubeAdmin.TektonController, &has.RetryOptions{Retries: 5, Always: true}, pipelineRun)).To(gomega.Succeed())

				// in case the first pipelineRun attempt has failed and was retried, we need to update the git branch head ref
				headSHA = pipelineRun.Labels["pipelinesascode.tekton.dev/sha"]
			})
		})

		ginkgo.When("Build PipelineRun completes successfully", ginkgo.Label(tsfTestLabel), func() {

			ginkgo.It("should validate Tekton TaskRun test results successfully", func() {
				pipelineRun, err = fw.AsKubeAdmin.HasController.GetComponentPipelineRun(component.GetName(), applicationName, userNamespace, headSHA)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(build.ValidateBuildPipelineTestResults(pipelineRun, fw.AsKubeAdmin.CommonController.KubeRest(), false)).To(gomega.Succeed())
			})

			ginkgo.It("should validate that the build pipelineRun is signed", func() {
				gomega.Eventually(func() error {
					pipelineRun, err = fw.AsKubeAdmin.HasController.GetComponentPipelineRun(component.GetName(), applicationName, userNamespace, headSHA)
					if err != nil {
						return err
					}
					if pipelineRun.Annotations["chains.tekton.dev/signed"] != "true" {
						return fmt.Errorf("pipelinerun %s/%s does not have the expected value of annotation 'chains.tekton.dev/signed'", pipelineRun.GetNamespace(), pipelineRun.GetName())
					}
					return nil
				}, time.Minute*5, time.Second*5).Should(gomega.Succeed(), "failed while validating build pipelineRun is signed")
			})

			ginkgo.It("should find the related Snapshot CR", func() {
				gomega.Eventually(func() error {
					snapshot, err = fw.AsKubeAdmin.IntegrationController.GetSnapshot("", pipelineRun.Name, "", userNamespace)
					return err
				}, snapshotTimeout, snapshotPollingInterval).Should(gomega.Succeed(), "timed out when trying to check if the Snapshot exists for PipelineRun %s/%s", userNamespace, pipelineRun.GetName())
			})

			ginkgo.It("should validate that the build pipelineRun is annotated with the name of the Snapshot", func() {
				pipelineRun, err = fw.AsKubeAdmin.HasController.GetComponentPipelineRun(component.GetName(), applicationName, userNamespace, headSHA)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(pipelineRun.Annotations["appstudio.openshift.io/snapshot"]).To(gomega.Equal(snapshot.GetName()))
			})

			ginkgo.It("should find the related Integration Test PipelineRun", func() {
				gomega.Eventually(func() error {
					testPipelinerun, err = fw.AsKubeAdmin.IntegrationController.GetIntegrationPipelineRun(integrationTestScenario.Name, snapshot.Name, userNamespace)
					if err != nil {
						ginkgo.GinkgoWriter.Printf("failed to get Integration test PipelineRun for a snapshot '%s' in '%s' namespace: %+v\n", snapshot.Name, userNamespace, err)
						return err
					}
					if !testPipelinerun.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", testPipelinerun.GetNamespace(), testPipelinerun.GetName())
					}
					return nil
				}, pipelineRunStartedTimeout, defaultPollingInterval).Should(gomega.Succeed())
				gomega.Expect(testPipelinerun.Labels["appstudio.openshift.io/snapshot"]).To(gomega.ContainSubstring(snapshot.Name))
				gomega.Expect(testPipelinerun.Labels["test.appstudio.openshift.io/scenario"]).To(gomega.ContainSubstring(integrationTestScenario.Name))
			})
		})

		ginkgo.When("Integration Test PipelineRun is created", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("should eventually complete successfully", func() {
				gomega.Expect(fw.AsKubeAdmin.IntegrationController.WaitForIntegrationPipelineToBeFinished(integrationTestScenario, snapshot, userNamespace)).To(gomega.Succeed(), fmt.Sprintf("Error when waiting for a integration pipeline for snapshot %s/%s to finish", userNamespace, snapshot.GetName()))
			})
		})

		ginkgo.When("Integration Test PipelineRun completes successfully", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("should lead to Snapshot CR being marked as passed", func() {
				gomega.Eventually(func() bool {
					snapshot, err = fw.AsKubeAdmin.IntegrationController.GetSnapshot("", pipelineRun.Name, "", userNamespace)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					return fw.AsKubeAdmin.CommonController.HaveTestsSucceeded(snapshot)
				}, time.Minute*5, defaultPollingInterval).Should(gomega.BeTrue(), fmt.Sprintf("tests have not succeeded for snapshot %s/%s", snapshot.GetNamespace(), snapshot.GetName()))
			})

			ginkgo.It("should trigger creation of a Release CR", func() {
				gomega.Eventually(func() error {
					release, err = fw.AsKubeAdmin.ReleaseController.GetRelease("", snapshot.Name, userNamespace)
					return err
				}, releaseTimeout, releasePollingInterval).Should(gomega.Succeed(), fmt.Sprintf("timed out when waiting for Release CR to be created for snapshot %s/%s", userNamespace, snapshot.GetName()))
			})
		})

		ginkgo.When("Release CR is created", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("triggers creation of Release PipelineRun in managed namespace", func() {
				gomega.Eventually(func() error {
					pipelineRun, err = fw.AsKubeAdmin.ReleaseController.GetPipelineRunInNamespace(managedNamespace, release.Name, release.Namespace)
					if err != nil {
						ginkgo.GinkgoWriter.Printf("Release PipelineRun not created yet for release '%s' in managed namespace '%s': %+v\n", release.Name, managedNamespace, err)
						return err
					}
					if !pipelineRun.HasStarted() {
						return fmt.Errorf("release pipelinerun %s/%s hasn't started yet", pipelineRun.GetNamespace(), pipelineRun.GetName())
					}
					return nil
				}, pipelineRunStartedTimeout, defaultPollingInterval).Should(gomega.Succeed(), fmt.Sprintf("failed to find started Release PipelineRun in managed namespace %q for release %q", managedNamespace, release.Name))
			})
		})

		ginkgo.When("Release PipelineRun is triggered", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("should eventually succeed", func() {
				gomega.Eventually(func() error {
					pr, err := fw.AsKubeAdmin.ReleaseController.GetPipelineRunInNamespace(managedNamespace, release.Name, release.Namespace)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					gomega.Expect(tekton.HasPipelineRunFailed(pr)).NotTo(gomega.BeTrue(), fmt.Sprintf("did not expect Release PipelineRun %s/%s to fail", pr.GetNamespace(), pr.GetName()))
					if !pr.IsDone() {
						return fmt.Errorf("release pipelinerun %s/%s has not finished yet", pr.GetNamespace(), pr.GetName())
					}
					gomega.Expect(tekton.HasPipelineRunSucceeded(pr)).To(gomega.BeTrue(), fmt.Sprintf("Release PipelineRun %s/%s did not succeed", pr.GetNamespace(), pr.GetName()))
					return nil
				}, releasePipelineTimeout, constants.PipelineRunPollingInterval).Should(gomega.Succeed(), fmt.Sprintf("release pipelinerun for release %q did not complete successfully", release.Name))
			})
		})

		ginkgo.When("Release PipelineRun is completed", ginkgo.Label(tsfTestLabel), func() {
			ginkgo.It("should lead to Release CR being marked as succeeded", func() {
				gomega.Eventually(func() error {
					release, err = fw.AsKubeAdmin.ReleaseController.GetRelease(release.Name, "", userNamespace)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
					if !release.IsReleased() {
						return fmt.Errorf("release CR %s/%s is not marked as released yet", release.GetNamespace(), release.GetName())
					}
					return nil
				}, customResourceUpdateTimeout, defaultPollingInterval).Should(gomega.Succeed(), fmt.Sprintf("release %q in namespace %q was not marked as released", release.Name, userNamespace))
			})
		})
	})
})

// createTsfReleaseConfig sets up release infrastructure in the managed namespace,
// following the pattern from setup-release.sh.
func createTsfReleaseConfig(fw *framework.Framework, managedNamespace, userNamespace, applicationName, componentName string) {
	var err error

	// Step 1: Create managed namespace with tenant label
	// Not using CreateTestNamespace — it adds ArgoCD/workspace labels and waits for
	// integration-runner SA, which is not appropriate for a managed namespace.
	nsObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: managedNamespace,
			Labels: map[string]string{
				"konflux-ci.dev/type": "tenant",
			},
		},
	}
	_, err = fw.AsKubeAdmin.CommonController.KubeInterface().CoreV1().Namespaces().Create(
		context.Background(), nsObj, metav1.CreateOptions{},
	)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(),
		"failed to create managed namespace %s", managedNamespace)

	// Step 2: Copy EnterpriseContractPolicy from enterprise-contract-service namespace
	defaultEcPolicy, err := fw.AsKubeAdmin.TektonController.GetEnterpriseContractPolicy(
		ecPolicySourceName, ecPolicySourceNS,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to get default ECP from %s/%s", ecPolicySourceNS, ecPolicySourceName)

	_, err = fw.AsKubeAdmin.TektonController.CreateEnterpriseContractPolicy(
		ecPolicySourceName, managedNamespace, defaultEcPolicy.Spec,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to create ECP in %s", managedNamespace)

	// Step 3: Create RoleBinding for system:authenticated -> ClusterRole/konflux-viewer-user-actions
	_, err = fw.AsKubeAdmin.CommonController.CreateRoleBinding(
		"viewer-rolebinding",
		managedNamespace,
		"Group",
		"system:authenticated",
		"",
		"ClusterRole",
		"konflux-viewer-user-actions",
		"rbac.authorization.k8s.io",
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to create viewer rolebinding in %s", managedNamespace)

	// Step 4: Create ImageRepository for trusted-artifacts
	trustedArtifactsIR := &imagecontrollerv1alpha1.ImageRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "trusted-artifacts",
			Namespace: managedNamespace,
		},
		Spec: imagecontrollerv1alpha1.ImageRepositorySpec{
			Image: imagecontrollerv1alpha1.ImageParameters{
				Name:       managedNamespace + "/trusted-artifacts",
				Visibility: imagecontrollerv1alpha1.ImageVisibilityPublic,
			},
		},
	}
	err = fw.AsKubeAdmin.CommonController.KubeRest().Create(
		context.Background(), trustedArtifactsIR,
	)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(),
		"failed to create trusted-artifacts ImageRepository")

	// Step 5: Create ImageRepository for the component
	componentIRName := "release-" + componentName
	componentIR := &imagecontrollerv1alpha1.ImageRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentIRName,
			Namespace: managedNamespace,
		},
		Spec: imagecontrollerv1alpha1.ImageRepositorySpec{
			Image: imagecontrollerv1alpha1.ImageParameters{
				Name:       managedNamespace + "/" + componentName,
				Visibility: imagecontrollerv1alpha1.ImageVisibilityPublic,
			},
		},
	}
	err = fw.AsKubeAdmin.CommonController.KubeRest().Create(
		context.Background(), componentIR,
	)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred(),
		"failed to create component ImageRepository %s", componentIRName)

	// Step 6: Wait for all ImageRepositories to become ready
	for _, irName := range []string{"trusted-artifacts", componentIRName} {
		ginkgo.GinkgoWriter.Printf("Waiting for ImageRepository %s/%s to become ready...\n",
			managedNamespace, irName)
		gomega.Eventually(func() (imagecontrollerv1alpha1.ImageRepositoryState, error) {
			ir, err := fw.AsKubeAdmin.ImageController.GetImageRepositoryCR(irName, managedNamespace)
			if err != nil {
				return "", err
			}
			return ir.Status.State, nil
		}, imageRepositoryReadyTimeout, defaultPollingInterval).Should(
			gomega.Equal(imagecontrollerv1alpha1.ImageRepositoryStateReady),
			fmt.Sprintf("ImageRepository %s/%s did not become ready", managedNamespace, irName),
		)
	}

	// Step 7: Fetch dynamic values from ImageRepository status
	taIR, err := fw.AsKubeAdmin.ImageController.GetImageRepositoryCR("trusted-artifacts", managedNamespace)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	taPushSecretName := taIR.Status.Credentials.PushSecretName
	taImageURL := taIR.Status.Image.URL
	ginkgo.GinkgoWriter.Printf("Trusted artifacts: pushSecret=%s, imageURL=%s\n",
		taPushSecretName, taImageURL)

	compIR, err := fw.AsKubeAdmin.ImageController.GetImageRepositoryCR(componentIRName, managedNamespace)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	compPushSecretName := compIR.Status.Credentials.PushSecretName
	compImageURL := compIR.Status.Image.URL
	ginkgo.GinkgoWriter.Printf("Component %s: pushSecret=%s, imageURL=%s\n",
		componentName, compPushSecretName, compImageURL)

	// Step 8: Create release-pipeline ServiceAccount with push secrets
	releaseSA, err := fw.AsKubeAdmin.CommonController.CreateServiceAccount(
		releasePipelineSAName, managedNamespace,
		[]corev1.ObjectReference{
			{Name: taPushSecretName},
			{Name: compPushSecretName},
		}, nil,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to create %s service account", releasePipelineSAName)

	// Step 9: Create RoleBinding for release-pipeline SA -> ClusterRole/release-pipeline-resource-role
	_, err = fw.AsKubeAdmin.ReleaseController.CreateReleasePipelineRoleBindingForServiceAccount(
		managedNamespace, releaseSA,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to create release pipeline role binding")

	// Step 10: Create ReleasePlanAdmission with per-component data mapping
	rpaData, err := json.Marshal(map[string]interface{}{
		"mapping": map[string]interface{}{
			"defaults": map[string]interface{}{
				"pushSourceContainer": false,
				"tags":                []string{"latest", "{{ git_sha }}"},
			},
			"components": []map[string]interface{}{
				{
					"name":       componentName,
					"repository": compImageURL,
				},
			},
		},
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	_, err = fw.AsKubeAdmin.ReleaseController.CreateReleasePlanAdmission(
		"tsf-release",
		managedNamespace,
		"",
		userNamespace,
		ecPolicySourceName,
		releasePipelineSAName,
		[]string{applicationName},
		false,
		&tektonutils.PipelineRef{
			Resolver: "git",
			Params: []tektonutils.Param{
				{Name: "url", Value: releaseServiceCatalogURL},
				{Name: "revision", Value: releaseServiceCatalogRevision},
				{Name: "pathInRepo", Value: releasePipelinePath},
			},
			OciStorage: taImageURL,
		},
		&runtime.RawExtension{Raw: rpaData},
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to create ReleasePlanAdmission")

	// Step 11: Create ReleasePlan in user namespace with auto-release enabled
	// Creating directly instead of using CreateReleasePlan helper because the helper
	// always sets standing-attribution to "true", but setup-release.sh uses "false".
	releasePlan := &releaseApi.ReleasePlan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tsf-release",
			Namespace: userNamespace,
			Labels: map[string]string{
				releaseMetadata.AutoReleaseLabel: "true",
				releaseMetadata.AttributionLabel: "true",
			},
		},
		Spec: releaseApi.ReleasePlanSpec{
			Application: applicationName,
			Target:      managedNamespace,
		},
	}
	err = fw.AsKubeAdmin.CommonController.KubeRest().Create(
		context.Background(), releasePlan,
	)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(),
		"failed to create ReleasePlan")

	ginkgo.GinkgoWriter.Printf("TSF release config created successfully in managed namespace %s\n",
		managedNamespace)
}
