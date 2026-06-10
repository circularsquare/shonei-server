package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// saves tests run against a fresh temp store. authSecret is set by auth_test's
// init(); we mint real Bearer tokens so the handlers exercise the same auth path
// as production.

func setupSaves(t *testing.T) {
	t.Helper()
	savesBaseDir = t.TempDir()
}

func saveTarget(slot string, q map[string]string) string {
	v := url.Values{}
	v.Set("slot", slot)
	for k, val := range q {
		v.Set(k, val)
	}
	return "/save?" + v.Encode()
}

// put drives handleSave (PUT) for user with a gzipped-opaque body + query meta.
func put(t *testing.T, user, slot string, body []byte, q map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, saveTarget(slot, q), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+makeToken(user))
	rec := httptest.NewRecorder()
	handleSave(rec, req)
	return rec
}

func get(t *testing.T, user, slot string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, saveTarget(slot, nil), nil)
	req.Header.Set("Authorization", "Bearer "+makeToken(user))
	rec := httptest.NewRecorder()
	handleSave(rec, req)
	return rec
}

func del(t *testing.T, user, slot string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, saveTarget(slot, nil), nil)
	req.Header.Set("Authorization", "Bearer "+makeToken(user))
	rec := httptest.NewRecorder()
	handleSave(rec, req)
	return rec
}

func list(t *testing.T, user string) []SaveMeta {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/saves", nil)
	req.Header.Set("Authorization", "Bearer "+makeToken(user))
	rec := httptest.NewRecorder()
	handleSavesList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/saves: got %d, want 200", rec.Code)
	}
	var metas []SaveMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &metas); err != nil {
		t.Fatalf("/saves decode: %v", err)
	}
	return metas
}

func TestSaveRoundTrip(t *testing.T) {
	setupSaves(t)
	blob := []byte("pretend-gzip-bytes\x00\x01\x02")
	rec := put(t, "anita", "myworld", blob, map[string]string{
		"savedAt": "1717890000", "animals": "9", "sv": "1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: got %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var meta SaveMeta
	json.Unmarshal(rec.Body.Bytes(), &meta)
	if meta.Rev != 1 || meta.AnimalCount != 9 || meta.SaveVersion != 1 {
		t.Fatalf("PUT meta wrong: %+v", meta)
	}

	g := get(t, "anita", "myworld")
	if g.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200", g.Code)
	}
	if !bytes.Equal(g.Body.Bytes(), blob) {
		t.Fatalf("GET body mismatch: got %q, want %q", g.Body.Bytes(), blob)
	}
	if ct := g.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("GET content-type %q, want octet-stream", ct)
	}
}

func TestRevIncrementsOnOverwrite(t *testing.T) {
	setupSaves(t)
	put(t, "anita", "w", []byte("a"), nil)
	rec := put(t, "anita", "w", []byte("bb"), map[string]string{"baseRev": "1"})
	var meta SaveMeta
	json.Unmarshal(rec.Body.Bytes(), &meta)
	if meta.Rev != 2 {
		t.Fatalf("rev after overwrite = %d, want 2", meta.Rev)
	}
	if meta.SizeGz != 2 {
		t.Fatalf("sizeGz = %d, want 2", meta.SizeGz)
	}
}

func TestListSkipsOtherAccounts(t *testing.T) {
	setupSaves(t)
	put(t, "anita", "a1", []byte("x"), nil)
	put(t, "anita", "a2", []byte("x"), nil)
	put(t, "bob", "b1", []byte("x"), nil)
	if got := len(list(t, "anita")); got != 2 {
		t.Fatalf("anita sees %d saves, want 2", got)
	}
	if got := len(list(t, "bob")); got != 1 {
		t.Fatalf("bob sees %d saves, want 1", got)
	}
}

func TestQuotaExceeded(t *testing.T) {
	setupSaves(t)
	saved := MaxAccountBytes
	MaxAccountBytes = 10
	defer func() { MaxAccountBytes = saved }()

	if rec := put(t, "anita", "w", []byte("12345"), nil); rec.Code != http.StatusOK {
		t.Fatalf("first PUT: got %d, want 200", rec.Code)
	}
	// Overwriting the same slot frees the old 5 bytes first, so 9 bytes fits.
	if rec := put(t, "anita", "w", []byte("123456789"), map[string]string{"baseRev": "1"}); rec.Code != http.StatusOK {
		t.Fatalf("overwrite within quota: got %d, want 200", rec.Code)
	}
	// A second slot pushes total over 10.
	if rec := put(t, "anita", "w2", []byte("1234567890ab"), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota PUT: got %d, want 413", rec.Code)
	}
}

func TestDeleteTombstonesAndBlocksResurrection(t *testing.T) {
	setupSaves(t)
	put(t, "anita", "w", []byte("orig"), nil) // rev 1
	if rec := del(t, "anita", "w"); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: got %d, want 200", rec.Code)
	}
	// Gone from listing + GET.
	if got := len(list(t, "anita")); got != 0 {
		t.Fatalf("after delete, list has %d, want 0", got)
	}
	if g := get(t, "anita", "w"); g.Code != http.StatusNotFound {
		t.Fatalf("GET deleted: got %d, want 404", g.Code)
	}
	// A stale client (baseRev <= tombstone rev) cannot resurrect it.
	if rec := put(t, "anita", "w", []byte("zombie"), map[string]string{"baseRev": "1"}); rec.Code != http.StatusConflict {
		t.Fatalf("resurrection PUT: got %d, want 409", rec.Code)
	}
	if got := len(list(t, "anita")); got != 0 {
		t.Fatalf("resurrection leaked a save: list has %d, want 0", got)
	}
}

func TestAutosaveRotation(t *testing.T) {
	setupSaves(t)
	saved := MaxCloudAutosaves
	MaxCloudAutosaves = 2
	defer func() { MaxCloudAutosaves = saved }()

	// Three autosaves with increasing savedAt; oldest should be pruned.
	put(t, "anita", "autosave 1", []byte("a"), map[string]string{"savedAt": "100"})
	put(t, "anita", "autosave 2", []byte("b"), map[string]string{"savedAt": "200"})
	put(t, "anita", "autosave 3", []byte("c"), map[string]string{"savedAt": "300"})

	metas := list(t, "anita")
	if len(metas) != 2 {
		t.Fatalf("autosaves kept = %d, want 2", len(metas))
	}
	for _, m := range metas {
		if m.Slot == "autosave 1" {
			t.Fatalf("oldest autosave was not pruned")
		}
	}
	// A manual save alongside autosaves is untouched by rotation.
	put(t, "anita", "manual", []byte("m"), map[string]string{"savedAt": "150"})
	if got := len(list(t, "anita")); got != 3 {
		t.Fatalf("manual + 2 autosaves = %d, want 3", got)
	}
}

func TestAuthRequired(t *testing.T) {
	setupSaves(t)
	req := httptest.NewRequest(http.MethodGet, "/saves", nil) // no Authorization
	rec := httptest.NewRecorder()
	handleSavesList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed /saves: got %d, want 401", rec.Code)
	}
}

func TestBadSlotRejected(t *testing.T) {
	setupSaves(t)
	for _, bad := range []string{"../etc", "a/b", "dot.dot", "pipe|x", ""} {
		req := httptest.NewRequest(http.MethodGet, "/save?slot="+bad, nil)
		req.Header.Set("Authorization", "Bearer "+makeToken("anita"))
		rec := httptest.NewRecorder()
		handleSave(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("slot %q: got %d, want 400", bad, rec.Code)
		}
	}
}

func TestQuotaAccountingMatchesBlobSize(t *testing.T) {
	setupSaves(t)
	put(t, "anita", "w", bytes.Repeat([]byte("x"), 1234), nil)
	if got := accountUsageBytes("anita"); got != 1234 {
		t.Fatalf("usage = %d, want 1234", got)
	}
	del(t, "anita", "w")
	if got := accountUsageBytes("anita"); got != 0 {
		t.Fatalf("usage after delete = %d, want 0 (tombstone has no blob)", got)
	}
}

// Sanity: a half-written upload (meta absent) is never listed.
func TestHalfWrittenBlobNotListed(t *testing.T) {
	setupSaves(t)
	put(t, "anita", "w", []byte("real"), nil)
	// Simulate a blob written with no meta by writing a stray blob directly.
	if err := writeFileAtomic(blobPath("anita", "orphan"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, m := range list(t, "anita") {
		if m.Slot == "orphan" {
			t.Fatalf("orphan blob (no meta) was listed")
		}
	}
}
