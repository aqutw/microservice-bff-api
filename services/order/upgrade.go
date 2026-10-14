package main

import "net/http"

// The old BFF could publish paid while its workflow still awaited inventory.
// Quarantine those orders before accepting traffic, then verify Payment through recovery.
func recoverLegacySettlements() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE orders SET status='settling' WHERE status='paid'
AND id IN (SELECT order_id FROM checkouts WHERE stage='awaiting_payment')`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT c.id,c.order_id FROM checkouts c JOIN orders o ON o.id=c.order_id
WHERE c.stage='awaiting_payment' AND o.status='settling' AND json_extract(c.result_json,'$.state')<>'settling'`)
	if err != nil {
		return err
	}
	type reference struct{ checkout, order string }
	var list []reference
	for rows.Next() {
		var item reference
		if err = rows.Scan(&item.checkout, &item.order); err != nil {
			rows.Close()
			return err
		}
		list = append(list, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range list {
		if _, err = tx.Exec(`UPDATE checkouts SET result_json=json_set(result_json,'$.state','settling','$.order.status','settling'),next_attempt=0 WHERE id=?`, item.checkout); err != nil {
			return err
		}
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-Actor-ID", "system")
		r.Header.Set("X-Correlation-ID", "settlement_upgrade")
		if err = recordAuditTx(tx, r, "order.settlement_recovering", "order", item.order, "recover_settlement", "success", map[string]any{"status": "paid"}, map[string]any{"status": "settling"}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
