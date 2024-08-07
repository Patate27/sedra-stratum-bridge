package sedrastratum

import (
	"context"
	"fmt"
	"time"

	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"
	"sedrastratum/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type SedraApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	sedrad        *rpcclient.RPCClient
	connected     bool
}

func NewSedraAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*SedraApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &SedraApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "sedraapi:"+address)),
		sedrad:        client,
		connected:     true,
	}, nil
}

func (ks *SedraApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *SedraApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.sedrad.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from sedra, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.sedrad.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from sedra, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *SedraApi) reconnect() error {
	if ks.sedrad != nil {
		return ks.sedrad.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.sedrad = client
	return nil
}

func (s *SedraApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking sedrad sync state")
	}
	for {
		clientInfo, err := s.sedrad.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from sedrad @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("Sedra is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("sedrad synced, starting server")
	}
	return nil
}

func (s *SedraApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	var blockReadyChan chan bool
	restartChannel := true
	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking sedrad sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to sedrad, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
			restartChannel = true
		}
		if restartChannel {
			blockReadyChan = make(chan bool)
			err := s.sedrad.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
				blockReadyChan <- true
			})
			if err != nil {
				s.logger.Error("fatal: failed to register for block notifications from sedra")
			} else {
				restartChannel = false
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (ks *SedraApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.sedrad.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via sedra-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from sedra")
	}
	return template, nil
}
