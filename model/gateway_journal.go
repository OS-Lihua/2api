package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// The journal delivers immutable intents to the primary database. It never
// changes balances and never infers completion from the age of a reservation.
var gatewayJournalMutex sync.Mutex

const maxGatewaySettlementBytes = 1 << 20

func gatewayJournalDirectory() string {
	if configured := os.Getenv("GATEWAY_SETTLEMENT_JOURNAL_DIR"); configured != "" {
		return configured
	}
	return filepath.Join(*common.LogDir, "gateway-settlements")
}

func gatewayJournalPath(requestID string) (string, error) {
	if requestID == "" || len(requestID) > 64 {
		return "", ErrLedgerInvalidEntry
	}
	for _, r := range requestID {
		if r < 'a' || r > 'z' {
			if r < 'A' || r > 'Z' {
				if (r < '0' || r > '9') && r != '-' && r != '_' {
					return "", ErrLedgerInvalidEntry
				}
			}
		}
	}
	return filepath.Join(gatewayJournalDirectory(), requestID+".json"), nil
}

func syncGatewayJournalDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// PreflightGatewaySettlementStorage rejects unwritable durable storage before
// any funds are reserved or any Provider is contacted.
func PreflightGatewaySettlementStorage(requestID string) error {
	if _, err := gatewayJournalPath(requestID); err != nil {
		return err
	}
	directory := filepath.Clean(gatewayJournalDirectory())
	var parents []string
	for path := directory; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(path); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parents = append(parents, filepath.Dir(path))
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	// Each newly created directory entry must reach its parent before funds move.
	for i := len(parents) - 1; i >= 0; i-- {
		if err := syncGatewayJournalDirectory(parents[i]); err != nil {
			return err
		}
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid gateway settlement directory")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(directory, ".preflight-")
	if err != nil {
		return err
	}
	defer os.Remove(probe.Name())
	err = probe.Sync()
	closeErr := probe.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Remove(probe.Name()); err != nil {
		return err
	}
	return syncGatewayJournalDirectory(directory)
}

func readGatewaySettlementJournal(path string) (GatewaySettlement, error) {
	var intent GatewaySettlement
	info, err := os.Lstat(path)
	if err != nil {
		return intent, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxGatewaySettlementBytes {
		return intent, fmt.Errorf("invalid gateway settlement journal")
	}
	file, err := os.Open(path)
	if err != nil {
		return intent, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxGatewaySettlementBytes+1))
	if err != nil {
		return intent, err
	}
	if len(body) > maxGatewaySettlementBytes {
		return intent, fmt.Errorf("gateway settlement journal exceeds limit")
	}
	if err := common.Unmarshal(body, &intent); err != nil {
		return intent, fmt.Errorf("invalid gateway settlement journal")
	}
	expected, err := gatewayJournalPath(intent.Request.ID)
	if err != nil || filepath.Clean(expected) != filepath.Clean(path) {
		return intent, ErrLedgerInvalidEntry
	}
	return intent, nil
}

func journalGatewaySettlement(intent GatewaySettlement) (GatewaySettlement, string, error) {
	gatewayJournalMutex.Lock()
	defer gatewayJournalMutex.Unlock()
	path, err := gatewayJournalPath(intent.Request.ID)
	if err != nil {
		return intent, "", err
	}
	existing, err := readGatewaySettlementJournal(path)
	if err == nil {
		if existing.Request.UserID != intent.Request.UserID {
			return intent, "", ErrLedgerInvalidEntry
		}
		return existing, path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return intent, "", err
	}
	body, err := common.Marshal(intent)
	if err != nil || len(body) > maxGatewaySettlementBytes {
		return intent, "", fmt.Errorf("invalid gateway settlement intent")
	}
	file, err := os.CreateTemp(gatewayJournalDirectory(), ".intent-")
	if err != nil {
		return intent, "", err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return intent, "", err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return intent, "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return intent, "", err
	}
	if err := file.Close(); err != nil {
		return intent, "", err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return intent, "", err
	}
	if err := syncGatewayJournalDirectory(gatewayJournalDirectory()); err != nil {
		return intent, "", err
	}
	return intent, path, nil
}

func removeGatewaySettlementJournal(path string) error {
	gatewayJournalMutex.Lock()
	defer gatewayJournalMutex.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncGatewayJournalDirectory(gatewayJournalDirectory())
}

func recoverGatewaySettlementJournals(ctx context.Context, limit int) error {
	directory, err := os.Open(gatewayJournalDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	for imported := 0; imported < limit; {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := directory.ReadDir(min(limit-imported, 100))
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			intent, err := readGatewaySettlementJournal(filepath.Join(gatewayJournalDirectory(), entry.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if err := PrepareGatewaySettlement(intent); err != nil {
				return err
			}
			imported++
		}
	}
	return nil
}
