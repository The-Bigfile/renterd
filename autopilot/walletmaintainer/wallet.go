package walletmaintainer

import (
	"context"
	"fmt"
	"sync"

	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/wallet"
	"go.sia.tech/renterd/v2/alerts"
	"go.sia.tech/renterd/v2/api"
	"go.sia.tech/renterd/v2/autopilot/contractor"
	"go.uber.org/zap"
)

type WalletMaintainerOption func(*walletMaintainer)

func WithNumOutputs(minNumOutputs, desiredNumOutputs uint64) WalletMaintainerOption {
	return func(w *walletMaintainer) {
		w.minNumOutputs = minNumOutputs
		w.desiredNumOutputs = desiredNumOutputs
	}
}

type (
	Bus interface {
		Wallet(ctx context.Context) (api.WalletResponse, error)
		WalletPending(ctx context.Context) (resp []wallet.Event, err error)
		WalletRedistribute(ctx context.Context, outputs int, amount types.Currency) (ids []types.TransactionID, err error)
	}
)

type (
	walletMaintainer struct {
		alerter alerts.Alerter
		bus     Bus
		logger  *zap.SugaredLogger

		minNumOutputs     uint64
		desiredNumOutputs uint64

		mu                sync.Mutex
		maintenanceTxnIDs []types.TransactionID
	}
)

func New(alerter alerts.Alerter, bus Bus, logger *zap.Logger, opts ...WalletMaintainerOption) *walletMaintainer {
	w := &walletMaintainer{
		alerter:           alerter,
		bus:               bus,
		minNumOutputs:     10,
		desiredNumOutputs: 100,
		logger:            logger.Named("wallet").Sugar(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

func (w *walletMaintainer) PerformWalletMaintenance(ctx context.Context, cfg api.AutopilotConfig) error {
	w.logger.Info("performing wallet maintenance")

	wallet, err := w.bus.Wallet(ctx)
	if err != nil {
		w.logger.Warnf("wallet maintenance skipped, fetching wallet failed with err: %v", err)
		return fmt.Errorf("failed to fetch wallet: %w", err)
	}

	// register an alert if balance is low
	balance := wallet.Confirmed
	if balance.Cmp(contractor.InitialContractFunding.Mul64(cfg.Contracts.Amount)) < 0 {
		if err := w.alerter.RegisterAlert(ctx, newAccountLowBalanceAlert(wallet.Address, balance, contractor.InitialContractFunding)); err != nil {
			w.logger.Warnf("failed to register low balance alert: %v", err)
		}
	} else {
		if err := w.alerter.DismissAlerts(ctx, alertLowBalanceID); err != nil {
			w.logger.Warnf("failed to dismiss low balance alert: %v", err)
		}
	}

	w.mu.Lock()
	maintenanceTxnIDs := w.maintenanceTxnIDs
	w.mu.Unlock()

	// pending maintenance transaction - nothing to do
	pending, err := w.bus.WalletPending(ctx)
	if err != nil {
		return nil
	}
	for _, txn := range pending {
		for _, mTxnID := range maintenanceTxnIDs {
			if mTxnID == types.TransactionID(txn.ID) {
				w.logger.Debugf("wallet maintenance skipped, pending transaction found with id %v", mTxnID)
				return nil
			}
		}
	}

	// calculate number of outputs
	amount := contractor.InitialContractFunding
	numOutputs := min(balance.Div(amount).Big().Uint64(), w.desiredNumOutputs)

	// skip maintenance if wallet balance is too low
	if numOutputs < w.minNumOutputs {
		w.logger.Warnf("wallet maintenance skipped, the balance of %v is too low to redistribute into outputs of %v, at a minimum we want to redistribute into %d outputs, so the balance should be at least %v", balance, amount, w.minNumOutputs, amount.Mul64(w.minNumOutputs))
		return nil
	}

	// redistribute outputs
	ids, err := w.bus.WalletRedistribute(ctx, int(numOutputs), amount)
	if err != nil {
		return fmt.Errorf("failed to redistribute wallet into %d outputs of amount %v, balance %v, err %v", int(numOutputs), amount, balance, err)
	}

	w.logger.Infof("wallet maintenance succeeded, txns %v", ids)

	w.mu.Lock()
	maintenanceTxnIDs = ids
	w.mu.Unlock()

	return nil
}
