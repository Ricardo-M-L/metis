package projectcoord

import "errors"

// ReleaseUnstartedClaim undoes a claim only while it has performed no durable
// provider or operation reservation. A paused/busy principal therefore never
// burns a task's recovery attempts, and a started execution cannot be erased.
func (s *Store) ReleaseUnstartedClaim(workspace, runID, itemID, worker, token string) (WorkItem, error) {
	var result WorkItem
	_, err := s.withRun(workspace, runID, func(run *Run) (bool, error) {
		item, err := run.item(itemID)
		if err != nil {
			return false, err
		}
		if err = item.assertClaim(worker, token); err != nil {
			return false, err
		}
		for _, r := range run.UsageRequests {
			if r.ItemID == itemID && r.ClaimToken == token {
				return false, errors.New("cannot release a claim after provider dispatch reservation")
			}
		}
		for _, r := range run.Operations {
			if r.ItemID == itemID && r.ClaimToken == token {
				return false, errors.New("cannot release a claim after operation reservation")
			}
		}
		archived, err := s.archivedClaimOperation(workspace, runID, run.OperationArchive, itemID, token)
		if err != nil {
			return false, err
		}
		if archived {
			return false, errors.New("cannot release a claim after archived operation reservation")
		}
		if len(item.Usage) == 0 || item.Usage[len(item.Usage)-1].Token != token || item.Usage[len(item.Usage)-1].SpentUSD != 0 {
			return false, errors.New("claim usage cannot be discarded")
		}
		item.Usage = item.Usage[:len(item.Usage)-1]
		item.Attempts--
		item.Status = ItemPending
		item.Owner = ""
		item.Lease = nil
		item.UpdatedAt = s.now()
		run.UpdatedAt = s.now()
		run.addEvent(s.now(), "claim_released_unstarted", itemID, "Principal did not authorize execution", worker)
		result = cloneItem(*item)
		return true, nil
	})
	return result, err
}
