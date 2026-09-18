package database

import "context"

// ListAstraPolicyStates is a single bounded-by-account-count overview refresh.
func (db *DB) ListAstraPolicyStates(ctx context.Context) ([]AstraPolicyState, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id,state_json,membership_version FROM codex_astra_policy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AstraPolicyState{}
	for rows.Next() {
		var id, version int64
		var raw string
		if err = rows.Scan(&id, &raw, &version); err != nil {
			return nil, err
		}
		s := AstraPolicyState{AccountID: id, OriginalGroupIDs: []int64{}}
		if err = decodeAstraState(raw, &s); err != nil {
			return nil, err
		}
		s.AccountID = id
		if s.OriginalGroupIDs == nil {
			s.OriginalGroupIDs = []int64{}
		}
		if s.Demoted && s.OwnedVersion != version {
			s.Demoted = false
			s.NextRecoveryAt = 0
			s.ConsecutiveFailures = 0
			s.Error = "manual_membership_changed"
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
