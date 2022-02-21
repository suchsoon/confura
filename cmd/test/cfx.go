package test

import (
	"math"
	"time"

	"github.com/conflux-chain/conflux-infura/test"
	"github.com/conflux-chain/conflux-infura/util"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	validConf test.EVConfig // epoch data validation configuration
	testCmd   = &cobra.Command{
		Use:   "cfx",
		Short: "Test/validate if infura epoch data complies with fullnode",
		Run:   startTest,
	}
)

func init() {
	testCmd.Flags().StringVarP(&validConf.FullnodeRpcEndpoint, "fn-endpoint", "f", "", "fullnode rpc endpoint used as benchmark")
	testCmd.MarkFlagRequired("fn-endpoint")

	testCmd.Flags().StringVarP(&validConf.InfuraRpcEndpoint, "infura-endpoint", "u", "", "infura rpc endpoint to be validated against")
	testCmd.MarkFlagRequired("infura-endpoint")

	testCmd.Flags().Uint64VarP(&validConf.EpochScanFrom, "start-epoch", "e", math.MaxUint64, "the epoch from where scan validation will start")
	testCmd.Flags().DurationVarP(&validConf.ScanInterval, "scan-interval", "c", 1*time.Second, "the interval for each scan validation")
	testCmd.Flags().DurationVarP(&validConf.SamplingInterval, "sampling-interval", "a", 10*time.Second, "the interval for each sampling validation")
	testCmd.Flags().StringVarP(&validConf.SamplingEpochType, "sampling-epoch-type", "t", "lc", "the epoch type for sampling validation: lm(latest_mined)/ls(latest_state)/lc(latest_confirmed)")

	Cmd.AddCommand(testCmd)
}

func startTest(cmd *cobra.Command, args []string) {
	if len(validConf.FullnodeRpcEndpoint) == 0 || len(validConf.InfuraRpcEndpoint) == 0 {
		logrus.Fatal("Fullnode && infura rpc endpoint must be configured for epoch data test/validation")
	}

	logrus.Info("Starting epoch data validator...")

	validator := test.MustNewEpochValidator(&validConf)
	defer validator.Destroy()

	util.StartAndGracefulShutdown(validator.Run)
}