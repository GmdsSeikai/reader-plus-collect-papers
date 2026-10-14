package reader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestCollectionImportBatchPartialFailure (R03) proves one failed item never
// blocks the rest of the batch and each item reports its own outcome.
func TestCollectionImportBatchPartialFailure(t *testing.T) {
	s := testServer(t)
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run CollectionRun
	json.Unmarshal(w.Body.Bytes(), &run)
	waitRunStatus(t, s, run.ID, runSucceeded)
	// Batch: a bad version, an unknown candidate, then the real one.
	w = request(t, s, "POST", "/api/collection/imports", strings.NewReader(`{"items":[
		{"arxivId":"2301.12345","version":0},
		{"arxivId":"2301.99999","version":1},
		{"arxivId":"2301.12345","version":2}
	]}`))
	var created struct {
		Items []struct {
			RequestID string `json:"requestId"`
			ArXivID   string `json:"arxivId"`
			Error     string `json:"error,omitempty"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || len(created.Items) != 3 {
		t.Fatalf("batch create = %s %v", w.Body.String(), err)
	}
	if created.Items[0].Error == "" || created.Items[1].Error == "" {
		t.Fatalf("invalid items must report their own error: %+v", created.Items)
	}
	if created.Items[2].RequestID == "" || created.Items[2].Error != "" {
		t.Fatalf("valid item lost to sibling failure: %+v", created.Items[2])
	}
	if status := waitImport(t, s, created.Items[2].RequestID, importImported); status != importImported {
		t.Fatalf("valid item did not import: %s", status)
	}
}

// TestCollectionImportTrashRestoreNeedsConfirm (R03) proves a duplicate whose
// document sits in the trash waits for the explicit restore condition and is
// never silently restored.
func TestCollectionImportTrashRestoreNeedsConfirm(t *testing.T) {
	s := testServer(t)
	data := pdfBytes(t)
	up := uploadTo(t, s, "papers", "paper.pdf", data)
	if up.Code != 201 {
		t.Fatalf("seed = %d %s", up.Code, up.Body.String())
	}
	var seeded Document
	if err := json.Unmarshal(up.Body.Bytes(), &seeded); err != nil {
		t.Fatal(err)
	}
	if w := request(t, s, "POST", "/api/documents/"+seeded.ID+"/trash", nil); w.Code != 200 {
		t.Fatalf("trash = %d", w.Code)
	}
	collectionFixture(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(collectionSearchAtom))
	})
	task := createCollectionTask(t, s, `{"name":"视觉","config":{"v":1,"keyword_groups":[["object detection"]],"group_operator_or":true,"authors":[],"categories":[],"latest_window_days":30,"hot_window_months":12,"frequency_hours":24}}`)
	w := request(t, s, "POST", "/api/collection/tasks/"+task.ID+"/runs", strings.NewReader("{}"))
	var run CollectionRun
	json.Unmarshal(w.Body.Bytes(), &run)
	waitRunStatus(t, s, run.ID, runSucceeded)
	w = request(t, s, "POST", "/api/collection/imports", strings.NewReader(`{"items":[{"arxivId":"2301.12345","version":2,"expectedContentHash":""}]}`))
	var created struct {
		Items []struct {
			RequestID string `json:"requestId"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	reqID := created.Items[0].RequestID
	if status := waitImport(t, s, reqID, importAwaitingConfirmation); status != importAwaitingConfirmation {
		t.Fatalf("trash duplicate did not wait: %s", status)
	}
	w = request(t, s, "GET", "/api/collection/imports/"+reqID, nil)
	var req CollectionImportRequest
	if err := json.Unmarshal(w.Body.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	if req.Condition != "trash-restore" {
		t.Fatalf("condition = %q, want trash-restore", req.Condition)
	}
	// Without confirm the trashed document stays in the trash.
	d, err := s.Store.Document(seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.DeletedAt == "" {
		t.Fatal("import silently restored the trashed document")
	}
	// Explicit confirmation restores and commits against the same document.
	w = request(t, s, "POST", "/api/collection/imports/"+reqID+"/confirm", nil)
	if w.Code != 202 {
		t.Fatalf("confirm = %d %s", w.Code, w.Body.String())
	}
	if status := waitImport(t, s, reqID, importImported); status != importImported {
		t.Fatalf("confirmed restore did not import: %s", status)
	}
	d, err = s.Store.Document(seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.DeletedAt != "" {
		t.Fatal("confirmed trash-restore did not restore the document")
	}
	if _, err := os.Stat(s.Store.File(d)); err != nil {
		t.Fatalf("restored document lost its file: %v", err)
	}
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])
	w = request(t, s, "GET", "/api/collection/imports/"+reqID, nil)
	json.Unmarshal(w.Body.Bytes(), &req)
	if req.ActualSHA256 != full {
		t.Fatalf("actual digest = %s want %s", req.ActualSHA256, full)
	}
}
