package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	_ "github.com/redhat-appstudio/tsf-cli/e2e/tests/tsf"

	"k8s.io/klog/v2"
)

func init() {
	klog.SetLogger(ginkgo.GinkgoLogr)

	verbosity := 1
	if v, err := strconv.ParseUint(os.Getenv("KLOG_VERBOSITY"), 10, 8); err == nil {
		verbosity = int(v)
	}

	flags := &flag.FlagSet{}
	klog.InitFlags(flags)
	if err := flags.Set("v", fmt.Sprintf("%d", verbosity)); err != nil {
		panic(err)
	}
}

func TestE2E(t *testing.T) {
	klog.Info("Starting TSF e2e tests...")
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "TSF E2E tests")
}
