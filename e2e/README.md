# TSF E2E Tests

End-to-end test suite for TSF (Trusted Software Factory) instances.

## Prerequisites

- **KUBECONFIG** pointing to a cluster with TSF installed
- **GitHub org** with a fork/clone of [konflux-ci/testrepo](https://github.com/konflux-ci/testrepo)
- **GitHub token** (PAT) with repo access to the org above
- **Quay org** accessible by the TSF instance's image-controller

## Running tests

1. Build the test binary:
   ```
   make build
   ```

2. Set up your env file:
   ```
   cp my-test.env.template my-test.env
   # edit my-test.env and fill in the values
   ```

3. Source the env file and run:
   ```
   source my-test.env
   ./bin/tsf.test --ginkgo.v --ginkgo.label-filter="tsf-demo"
   ```
