package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

const (
	notifyTypeWalletReconcile = "wallet_reconcile"

	walletReconcileBatchSize     = 500
	walletReconcileAlertedWallet = 20
)

// WalletReconcileHandler is the scheduled job that checks every wallet
// balance against the sum of its ledger entries. A mismatch fails the run,
// logs each wallet and notifies the root user. WALLET_RECONCILE_ENABLED
// (default true) and WALLET_RECONCILE_INTERVAL_MINUTES (default 60) control
// it; the interval is measured on pkg/clock.
type WalletReconcileHandler struct{}

func (WalletReconcileHandler) Type() string { return model.SystemTaskTypeWalletReconcile }

func (WalletReconcileHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("WALLET_RECONCILE_ENABLED", true)
}

func (WalletReconcileHandler) Interval() time.Duration {
	minutes := common.GetEnvOrDefault("WALLET_RECONCILE_INTERVAL_MINUTES", 60)
	if minutes <= 0 {
		minutes = 60
	}
	return time.Duration(minutes) * time.Minute
}

func (WalletReconcileHandler) NewPayload() any { return nil }

func (WalletReconcileHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	result, err := model.ReconcileWallets(walletReconcileBatchSize)
	if err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	status, message := model.SystemTaskStatusSucceeded, ""
	if len(result.Mismatches) > 0 {
		alertWalletMismatches(ctx, result.Mismatches)
		status = model.SystemTaskStatusFailed
		message = fmt.Sprintf("%d of %d wallets do not match their ledger entries", len(result.Mismatches), result.CheckedWallets)
	}
	if len(result.Mismatches) > walletReconcileAlertedWallet {
		result.Mismatches = result.Mismatches[:walletReconcileAlertedWallet]
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, status, result, message); err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}

func alertWalletMismatches(ctx context.Context, mismatches []model.WalletMismatch) {
	ids := make([]string, 0, min(len(mismatches), walletReconcileAlertedWallet))
	for i, mismatch := range mismatches {
		logger.LogError(ctx, common.LogText("wallet %d (%s %d) balance %d %s does not equal its ledger total %s, foreign-currency entries %d",
			mismatch.WalletId, mismatch.AccountType, mismatch.OwnerId, mismatch.Balance.Nanos, mismatch.Balance.Currency,
			mismatch.EntryTotalNanos.String(), mismatch.ForeignCurrencyEntries))
		if i < walletReconcileAlertedWallet {
			ids = append(ids, strconv.FormatInt(mismatch.WalletId, 10))
		}
	}
	params := map[string]any{"Count": len(mismatches), "Shown": len(ids), "Ids": strings.Join(ids, ", ")}
	notifyAdmins(notifyTypeWalletReconcile, func(role int, _ dto.UserSetting) bool {
		return role == common.RoleRootUser
	}, func(lang string) (string, string) {
		return i18n.Translate(lang, i18n.MsgWalletReconcileMismatchSubject, params), i18n.Translate(lang, i18n.MsgWalletReconcileMismatchContent, params)
	})
}
