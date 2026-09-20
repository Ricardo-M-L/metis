package main

import "testing"

func TestWorkerReportedBlockedRequiresStandaloneStatusLine(t *testing.T) {
	if workerReportedBlocked("The instructions say to finish with `METIS_PROJECT_STATUS: blocked` only when blocked.") {
		t.Fatal("quoted prompt marker must not block completed work")
	}
	if !workerReportedBlocked("Could not reach the service.\nMETIS_PROJECT_STATUS: blocked\n") {
		t.Fatal("standalone status marker must block the work item")
	}
	if !workerReportedBlocked("details\n  `METIS_PROJECT_STATUS: blocked` because credentials are unavailable") {
		t.Fatal("indented standalone status marker must block the work item")
	}
}
