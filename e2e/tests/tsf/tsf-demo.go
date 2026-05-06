package tsf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	buildcontrollers "github.com/konflux-ci/build-service/controllers"

	"github.com/devfile/library/v2/pkg/util"
	"github.com/google/go-github/v44/github"
	appservice "github.com/konflux-ci/application-api/api/v1alpha1"
	"github.com/konflux-ci/e2e-tests/pkg/clients/has"
	"github.com/konflux-ci/e2e-tests/pkg/constants"
	"github.com/konflux-ci/e2e-tests/pkg/framework"
	"github.com/konflux-ci/e2e-tests/pkg/utils"
	"github.com/konflux-ci/e2e-tests/pkg/utils/build"
	"github.com/konflux-ci/e2e-tests/pkg/utils/tekton"
	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"

	integrationv1beta2 "github.com/konflux-ci/integration-service/api/v1beta2"
	releaseApi "github.com/konflux-ci/release-service/api/v1alpha1"
	tektonapi "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"k8s.io/apimachinery/pkg/types"
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
	customResourceUpdateTimeout = time.Minute * 10

	// Intervals
	defaultPollingInterval  = time.Second * 2
	snapshotPollingInterval = time.Second * 1
	releasePollingInterval  = time.Second * 1

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

			// Managed namespace is set up externally (setup-release.sh) and passed via env var
			managedNamespace = os.Getenv("TSF_MANAGED_NAMESPACE")
			gomega.Expect(managedNamespace).NotTo(gomega.BeEmpty(), "TSF_MANAGED_NAMESPACE env var is not set")

			// Application and component names must match what setup-release.sh was called with
			applicationName = os.Getenv("TSF_APPLICATION_NAME")
			gomega.Expect(applicationName).NotTo(gomega.BeEmpty(), "TSF_APPLICATION_NAME env var is not set")

			componentName = os.Getenv("TSF_COMPONENT_NAME")
			gomega.Expect(componentName).NotTo(gomega.BeEmpty(), "TSF_COMPONENT_NAME env var is not set")

			pacBranchName = fmt.Sprintf("%s%s", constants.PaCPullRequestBranchPrefix, componentName)
			componentRepositoryName = utils.ExtractGitRepositoryNameFromURL(gitSourceUrl)

			// Get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuildOciTAMin)
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
				gomega.Expect(build.ValidateBuildPipelineTestResults(pipelineRun, fw.AsKubeAdmin.CommonController.KubeRest(), false, true)).To(gomega.Succeed())
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
				err := fw.AsKubeAdmin.IntegrationController.WaitForIntegrationPipelineToBeFinished(integrationTestScenario, snapshot, userNamespace)
				if err != nil {
					if plr, plrErr := fw.AsKubeAdmin.IntegrationController.GetIntegrationPipelineRun(integrationTestScenario.Name, snapshot.Name, userNamespace); plrErr == nil {
						for _, chr := range plr.Status.ChildReferences {
							taskRun := &tektonapi.TaskRun{}
							if trErr := fw.AsKubeAdmin.CommonController.KubeRest().Get(context.Background(), types.NamespacedName{Namespace: plr.Namespace, Name: chr.Name}, taskRun); trErr != nil {
								continue
							}
							for _, cond := range taskRun.Status.Conditions {
								if cond.Reason != "Failed" {
									continue
								}
								for _, step := range taskRun.Status.Steps {
									logs, logErr := utils.GetContainerLogs(fw.AsKubeAdmin.CommonController.KubeInterface(), taskRun.Status.PodName, step.Container, plr.Namespace)
									if logErr == nil && logs != "" {
										ginkgo.GinkgoWriter.Printf("\n=== Logs from %s/%s ===\n%s\n", chr.PipelineTaskName, step.Container, logs)
									}
								}
							}
						}
					}
				}
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred(), fmt.Sprintf("Error when waiting for a integration pipeline for snapshot %s/%s to finish", userNamespace, snapshot.GetName()))
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
					if tekton.HasPipelineRunFailed(pr) {
						failLogs, _ := tekton.GetFailedPipelineRunLogs(
							fw.AsKubeAdmin.CommonController.KubeRest(),
							fw.AsKubeAdmin.CommonController.KubeInterface(),
							pr,
						)
						ginkgo.GinkgoWriter.Printf("=== FAILED PIPELINE RUN LOGS ===\n%s\n", failLogs)
						gomega.Expect(true).To(gomega.BeFalse(),
							fmt.Sprintf("Release PipelineRun %s/%s failed. See diagnostics above.", pr.GetNamespace(), pr.GetName()))
					}
					if !pr.IsDone() {
						return fmt.Errorf("release pipelinerun %s/%s has not finished yet", pr.GetNamespace(), pr.GetName())
					}
					gomega.Expect(tekton.HasPipelineRunSucceeded(pr)).To(gomega.BeTrue(),
						fmt.Sprintf("Release PipelineRun %s/%s did not succeed", pr.GetNamespace(), pr.GetName()))
					return nil
				}, releasePipelineTimeout, constants.PipelineRunPollingInterval).Should(gomega.Succeed(),
					fmt.Sprintf("release pipelinerun for release %q did not complete successfully", release.Name))
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

