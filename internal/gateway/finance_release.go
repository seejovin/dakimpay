package gateway

import (
	"fmt"

	"dakimpay/internal/gwstore"
	"dakimpay/internal/railcontract"
)

type releaseGuard struct {
	OpenCases      int64 `json:"open_cases"`
	SourceRefunded int64 `json:"source_refunded_minor"`
}

func releaseAnchor(capture journalEntry) string {
	// A second external collection links back to the original captured operation.
	if capture.Source != "" && capture.Parent != "" {
		return capture.Parent
	}
	return capture.Reference
}

func changeReleaseGuard(tx gwstore.Tx, prefix, ref string, caseDelta, refundedDelta int64) error {
	key := bookKey(prefix, "release-guard", ref)
	var g releaseGuard
	_, err := gwstore.GetJSON(tx, key, &g)
	if err != nil {
		return err
	}
	g.OpenCases, err = addFinancialAmount(g.OpenCases, caseDelta)
	if err != nil {
		return err
	}
	if g.OpenCases < 0 {
		return fmt.Errorf("inconsistent outstanding financial case count")
	}
	g.SourceRefunded, err = addFinancialAmount(g.SourceRefunded, refundedDelta)
	if err != nil {
		return err
	}
	return gwstore.PutJSON(tx, key, g)
}

func openReleaseCase(tx gwstore.Tx, prefix, kind, id, original string, amount int64) error {
	key := bookKey(prefix, kind, id)
	if _, found, err := tx.Get(key); err != nil {
		return err
	} else if found {
		return financialConflict("financial exception already exists")
	}
	if err := changeReleaseGuard(tx, prefix, original, 1, 0); err != nil {
		return err
	}
	return gwstore.PutJSON(tx, key, captureException{Original: original, Amount: amount})
}

func restoreReleaseCase(tx gwstore.Tx, prefix, kind, id string, amount int64) error {
	key := bookKey(prefix, kind, id)
	var x captureException
	found, err := gwstore.GetJSON(tx, key, &x)
	if err != nil {
		return err
	}
	if !found {
		return financialConflict("adjustment lacks its original financial exception")
	}
	wasOpen := x.Refunded != x.Amount
	x.Refunded, err = addFinancialAmount(x.Refunded, amount)
	if err != nil {
		return err
	}
	nowOpen := x.Refunded != x.Amount
	if wasOpen != nowOpen {
		delta := int64(-1)
		if nowOpen {
			delta = 1
		}
		if err = changeReleaseGuard(tx, prefix, x.Original, delta, 0); err != nil {
			return err
		}
	}
	return gwstore.PutJSON(tx, key, x)
}

// releaseFinancialHold is a transactional predicate, for use alongside the
// merchant release / voluntary refund decision in the same transaction. Any
// independently evidenced change to the original capture is serialized with it.
func releaseFinancialHold(tx gwstore.Tx, c railcontract.Command, ref string) (bool, error) {
	prefix := bookPrefix(c.Merchant, c.Account, c.Currency)
	route := FinancialRoute{RouteID: c.RouteID, ProviderAccount: c.ProviderAccount}
	entry, ok, err := readPosting(tx, prefix, route, "capture", ref)
	if err != nil {
		return true, err
	}
	if !ok || c.Kind != "charge" || entry.Operation != c.ID || entry.Order != c.Order || entry.Parent != "" ||
		entry.Legs[0].Account != processorClearing || entry.Legs[0].Debit != c.Amount {
		return true, nil
	}
	var g releaseGuard
	_, err = gwstore.GetJSON(tx, bookKey(route.sourcePrefix(prefix), "release-guard", ref), &g)
	return err != nil || g.OpenCases != 0 || g.SourceRefunded != 0, err
}
