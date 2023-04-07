package cmd

import (
	"context"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Conflux-Chain/confura/cmd/util"
	"github.com/Conflux-Chain/confura/rpc/handler"
	"github.com/Conflux-Chain/confura/store/mysql"
	rpcutil "github.com/Conflux-Chain/confura/util/rpc"
	"github.com/Conflux-Chain/confura/virtualfilter"
)

var (
	virtualFilterCmd = &cobra.Command{
		Use:   "vf",
		Short: "Start virtual filter service (for eSpace only)",
		Run:   startVirtualFilterService,
	}
)

func init() {
	rootCmd.AddCommand(virtualFilterCmd)
}

func startVirtualFilterService(*cobra.Command, []string) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	storeCtx := util.MustInitStoreContext()
	defer storeCtx.Close()

	startEvmSpaceVirtualFilterServer(ctx, &wg, storeCtx)

	util.GracefulShutdown(&wg, cancel)
}

// startEvmSpaceVirtualFilterServer starts virtual filter RPC server (eSpace supported only for now)
func startEvmSpaceVirtualFilterServer(ctx context.Context, wg *sync.WaitGroup, storeCtx util.StoreContext) {
	var vfls *mysql.VirtualFilterLogStore
	var logApiHandler *handler.EthLogsApiHandler

	if storeCtx.EthDB != nil {
		vfls = storeCtx.EthDB.VirtualFilterLogStore
		logApiHandler = handler.NewEthLogsApiHandler(storeCtx.EthDB)
	}

	shutdownCtx := util.GracefulShutdownContext{Ctx: ctx, Wg: wg}

	// serve HTTP endpoint
	vfServer, httpEndpoint := virtualfilter.MustNewEvmSpaceServerFromViper(
		shutdownCtx, vfls, logApiHandler,
	)

	go vfServer.MustServeGraceful(ctx, wg, httpEndpoint, rpcutil.ProtocolHttp)
}
