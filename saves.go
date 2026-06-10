package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Account-owned cloud saves — Phase 2 of the account system (see
// plans/account-system.md). Pure HTTP, decoupled from the trading WS / Hub.
//
// Model: the client's local save stays authoritative; this is an async MIRROR.
// The client uploads the gzipped save blob; we store it opaquely (we never
// decompress — gzip is the client's concern) alongside a small uncompressed
// metadata sidecar so listing never has to touch the blob.
//
// Layout (savesBaseDir is a sibling of accounts.json, override via env):
//   <savesBaseDir>/<username>/<slot>.json.gz     gzipped world blob
//   <savesBaseDir>/<username>/<slot>.meta.json   SaveMeta sidecar (the commit marker)
//
// Identity is the token's username (Authorization: Bearer <token>), reusing
// verifyToken from auth.go. Usernames are already path-safe (usernameRe); slot
// names are sanitized here.
//
// Conflict policy is last-write-wins (the "visible" half lives client-side: the
// menu badges cloud-vs-local before a player overwrites). Server-assigned `rev`
// is the monotonic source of truth for "did the cloud move". The ONE case we
// reject is resurrecting a tombstoned slot from a stale client (see PUT).
// ---------------------------------------------------------------------------

const (
	// SavesDir is the default base directory, written next to accounts.json and
	// deliberately NOT shipped by deploy (like accounts/pricelog) so a redeploy
	// never wipes player saves. Override with SHONEI_SAVES_DIR.
	SavesDir = "saves"

	// MaxSaveBytes caps a single PUT body (gzipped). A real save is ~0.1-0.3 MB
	// gzipped; this is a generous hard ceiling against a runaway upload.
	MaxSaveBytes = 8 << 20 // 8 MB

	// autosaveSlotPrefix mirrors SaveSystem.AutosavePrefix on the client.
	autosaveSlotPrefix = "autosave"

	blobSuffix = ".json.gz"
	metaSuffix = ".meta.json"
)

// Quota knobs — vars (not consts) so tests can shrink them.
var (
	// MaxAccountBytes caps the sum of a player's stored blobs. Generous given
	// gzipped saves, but bounds disk on the small box. Enforced on PUT → 413.
	MaxAccountBytes int64 = 50 << 20 // 50 MB

	// MaxCloudAutosaves is how many "autosave *" blobs we keep per account,
	// pruned server-side at PUT time. Client-side rotation can't bound this:
	// autosave names are per-machine timestamps, so each machine only deletes
	// its own and cloud autosaves would otherwise grow unbounded across devices.
	MaxCloudAutosaves = 5
)

// slotRe bounds slot names to a filesystem-safe charset. No '.' → ".." can't be
// formed; spaces are allowed because existing autosave names contain them
// ("autosave 2026-06-08 16-02-00").
var slotRe = regexp.MustCompile(`^[A-Za-z0-9 _\-]{1,64}$`)

// SaveMeta is the per-slot sidecar. Written as the commit marker AFTER the blob,
// so a blob with no meta is treated as a half-written upload (absent).
type SaveMeta struct {
	Slot        string `json:"slot"`
	Rev         int64  `json:"rev"`         // server-assigned, monotonic per slot
	SavedAt     int64  `json:"savedAt"`     // client wall clock, unix sec — display/tiebreak only
	SizeGz      int64  `json:"sizeGz"`      // stored blob size in bytes (drives quota)
	AnimalCount int    `json:"animalCount"` // shown in the menu without decompressing
	SaveVersion int    `json:"saveVersion"` // WorldSaveData.saveVersion — gates downloads to older clients
	Origin      string `json:"origin,omitempty"` // client machine GUID, for the conflict UI
	Deleted     bool   `json:"deleted,omitempty"` // tombstone: slot deleted at Rev, blob removed
}

// ── Config + per-user locking ───────────────────────────────────────────────

var savesBaseDir = SavesDir

// userLocks serializes mutations per account so two concurrent PUTs can't race
// on the quota calculation or the blob/meta write pair.
var (
	userLocksMu sync.Mutex
	userLocks   = map[string]*sync.Mutex{}
)

func initSaves() {
	if d := os.Getenv("SHONEI_SAVES_DIR"); d != "" {
		savesBaseDir = d
	}
	if err := os.MkdirAll(savesBaseDir, 0700); err != nil {
		log.Printf("[saves] mkdir base %q failed: %v", savesBaseDir, err)
	}
	log.Printf("[saves] store at %q (per-account quota %d MB, keep %d autosaves)",
		savesBaseDir, MaxAccountBytes>>20, MaxCloudAutosaves)
}

func userLock(username string) *sync.Mutex {
	userLocksMu.Lock()
	defer userLocksMu.Unlock()
	m, ok := userLocks[username]
	if !ok {
		m = &sync.Mutex{}
		userLocks[username] = m
	}
	return m
}

// ── Path helpers ─────────────────────────────────────────────────────────────

func userSaveDir(username string) string { return filepath.Join(savesBaseDir, username) }

func blobPath(username, slot string) string {
	return filepath.Join(userSaveDir(username), slot+blobSuffix)
}
func metaPath(username, slot string) string {
	return filepath.Join(userSaveDir(username), slot+metaSuffix)
}

// sanitizeSlot validates a client-supplied slot name. Returns false (and writes
// a 400) on anything outside slotRe.
func sanitizeSlot(w http.ResponseWriter, slot string) (string, bool) {
	if !slotRe.MatchString(slot) {
		writeAuthError(w, http.StatusBadRequest, "bad slot name")
		return "", false
	}
	return slot, true
}

// ── Auth ─────────────────────────────────────────────────────────────────────

// authedUsername resolves the caller's identity from an Authorization: Bearer
// header (reusing verifyToken). Header rather than ?token= keeps tokens out of
// proxy logs/caches on these REST calls. Writes a 401 and returns false on
// failure. In insecure dev mode an `?name=` fallback keeps the CLI usable.
func authedUsername(w http.ResponseWriter, r *http.Request) (string, bool) {
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
		name, err := verifyToken(strings.TrimSpace(h[len(prefix):]))
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid token")
			return "", false
		}
		return name, true
	}
	if insecureMode {
		if name := r.URL.Query().Get("name"); name != "" {
			return strings.ToLower(name), true
		}
	}
	writeAuthError(w, http.StatusUnauthorized, "missing bearer token")
	return "", false
}

// ── Meta I/O ─────────────────────────────────────────────────────────────────

var errNoMeta = errors.New("no meta")

// readMeta loads one slot's sidecar. errNoMeta if absent.
func readMeta(username, slot string) (SaveMeta, error) {
	var m SaveMeta
	data, err := os.ReadFile(metaPath(username, slot))
	if err != nil {
		if os.IsNotExist(err) {
			return m, errNoMeta
		}
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// writeFileAtomic writes via temp file + rename (the pricelog.go/auth.go pattern).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeMeta(username string, m SaveMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(metaPath(username, m.Slot), data, 0600)
}

// listMetas returns every non-half-written slot meta for an account (including
// tombstones — callers filter). A meta with no blob and Deleted=false is a
// half-written upload and is skipped.
func listMetas(username string) []SaveMeta {
	dir := userSaveDir(username)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // missing dir → no saves
	}
	var out []SaveMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), metaSuffix) {
			continue
		}
		slot := strings.TrimSuffix(e.Name(), metaSuffix)
		m, err := readMeta(username, slot)
		if err != nil {
			continue
		}
		if !m.Deleted {
			if _, err := os.Stat(blobPath(username, slot)); err != nil {
				continue // meta without blob and not a tombstone → half-written
			}
		}
		out = append(out, m)
	}
	return out
}

// accountUsageBytes sums stored blob sizes (tombstones have none).
func accountUsageBytes(username string) int64 {
	var total int64
	for _, m := range listMetas(username) {
		total += m.SizeGz
	}
	return total
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// handleSavesList — GET /saves → live (non-tombstoned) slot metadata.
func handleSavesList(w http.ResponseWriter, r *http.Request) {
	username, ok := authedUsername(w, r)
	if !ok {
		return
	}
	live := make([]SaveMeta, 0)
	for _, m := range listMetas(username) {
		if !m.Deleted {
			live = append(live, m)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(live)
}

// handleSave routes GET/PUT/DELETE /save?slot=.
func handleSave(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleSaveGet(w, r)
	case http.MethodPut:
		handleSavePut(w, r)
	case http.MethodDelete:
		handleSaveDelete(w, r)
	default:
		writeAuthError(w, http.StatusMethodNotAllowed, "GET/PUT/DELETE only")
	}
}

// handleSaveGet — download one slot's gzipped blob as opaque octet-stream. We do
// NOT set Content-Encoding: gzip (proxies / UnityWebRequest would transparently
// inflate it); the client gunzips the body itself.
func handleSaveGet(w http.ResponseWriter, r *http.Request) {
	username, ok := authedUsername(w, r)
	if !ok {
		return
	}
	slot, ok := sanitizeSlot(w, r.URL.Query().Get("slot"))
	if !ok {
		return
	}
	m, err := readMeta(username, slot)
	if err != nil || m.Deleted {
		writeAuthError(w, http.StatusNotFound, "no such save")
		return
	}
	data, err := os.ReadFile(blobPath(username, slot))
	if err != nil {
		writeAuthError(w, http.StatusNotFound, "no such save")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Save-Rev", strconv.FormatInt(m.Rev, 10))
	w.Write(data)
}

// handleSavePut — upload a slot. Query: slot, baseRev, savedAt, animals, sv, origin.
// Body: gzipped JSON (opaque). Last-write-wins, EXCEPT a stale client may not
// resurrect a tombstoned slot (baseRev <= tombstone rev → 409).
func handleSavePut(w http.ResponseWriter, r *http.Request) {
	username, ok := authedUsername(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	slot, ok := sanitizeSlot(w, q.Get("slot"))
	if !ok {
		return
	}

	body, err := readAllLimited(w, r, MaxSaveBytes)
	if err != nil {
		return // readAllLimited already wrote the response (413/400)
	}

	baseRev := parseInt64(q.Get("baseRev"), 0)
	lock := userLock(username)
	lock.Lock()
	defer lock.Unlock()

	prev, perr := readMeta(username, slot)
	if perr == nil && prev.Deleted && baseRev <= prev.Rev {
		// Stale client trying to revive a slot deleted elsewhere. Reject so the
		// delete sticks; the client surfaces "deleted on another device".
		writeAuthError(w, http.StatusConflict, "save deleted on another device")
		return
	}

	// Quota: replacing this slot frees its old blob first.
	var prevSize int64
	if perr == nil {
		prevSize = prev.SizeGz
	}
	if accountUsageBytes(username)-prevSize+int64(len(body)) > MaxAccountBytes {
		writeAuthError(w, http.StatusRequestEntityTooLarge, "account save quota exceeded")
		return
	}

	if err := os.MkdirAll(userSaveDir(username), 0700); err != nil {
		log.Printf("[saves] mkdir %q failed: %v", userSaveDir(username), err)
		writeAuthError(w, http.StatusInternalServerError, "store unavailable")
		return
	}

	newRev := int64(1)
	if perr == nil {
		newRev = prev.Rev + 1
	}
	// Blob first, meta second: meta is the commit marker.
	if err := writeFileAtomic(blobPath(username, slot), body, 0600); err != nil {
		log.Printf("[saves] write blob %s/%s failed: %v", username, slot, err)
		writeAuthError(w, http.StatusInternalServerError, "write failed")
		return
	}
	meta := SaveMeta{
		Slot:        slot,
		Rev:         newRev,
		SavedAt:     parseInt64(q.Get("savedAt"), 0),
		SizeGz:      int64(len(body)),
		AnimalCount: int(parseInt64(q.Get("animals"), 0)),
		SaveVersion: int(parseInt64(q.Get("sv"), 0)),
		Origin:      q.Get("origin"),
	}
	if err := writeMeta(username, meta); err != nil {
		log.Printf("[saves] write meta %s/%s failed: %v", username, slot, err)
		writeAuthError(w, http.StatusInternalServerError, "write failed")
		return
	}

	if strings.HasPrefix(slot, autosaveSlotPrefix) {
		pruneAutosaves(username)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meta)
}

// handleSaveDelete — tombstone a slot: bump rev, mark Deleted, remove the blob.
// The tombstone (a meta with no blob) blocks stale-client resurrection.
func handleSaveDelete(w http.ResponseWriter, r *http.Request) {
	username, ok := authedUsername(w, r)
	if !ok {
		return
	}
	slot, ok := sanitizeSlot(w, r.URL.Query().Get("slot"))
	if !ok {
		return
	}
	lock := userLock(username)
	lock.Lock()
	defer lock.Unlock()

	prev, perr := readMeta(username, slot)
	rev := int64(1)
	if perr == nil {
		if prev.Deleted {
			w.WriteHeader(http.StatusOK) // already tombstoned — idempotent
			return
		}
		rev = prev.Rev + 1
	}
	tomb := SaveMeta{Slot: slot, Rev: rev, Deleted: true}
	if err := writeMeta(username, tomb); err != nil {
		log.Printf("[saves] tombstone %s/%s failed: %v", username, slot, err)
		writeAuthError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if err := os.Remove(blobPath(username, slot)); err != nil && !os.IsNotExist(err) {
		log.Printf("[saves] remove blob %s/%s failed: %v", username, slot, err)
	}
	w.WriteHeader(http.StatusOK)
}

// pruneAutosaves keeps only the newest MaxCloudAutosaves "autosave *" blobs for
// an account (by SavedAt). Pruned autosaves are removed outright (not tombstoned)
// — they're disposable and per-machine-named, so there's nothing to resurrect.
// Caller must hold the user lock.
func pruneAutosaves(username string) {
	var autos []SaveMeta
	for _, m := range listMetas(username) {
		if !m.Deleted && strings.HasPrefix(m.Slot, autosaveSlotPrefix) {
			autos = append(autos, m)
		}
	}
	if len(autos) <= MaxCloudAutosaves {
		return
	}
	sort.Slice(autos, func(i, j int) bool { return autos[i].SavedAt > autos[j].SavedAt }) // newest first
	for _, m := range autos[MaxCloudAutosaves:] {
		os.Remove(blobPath(username, m.Slot))
		os.Remove(metaPath(username, m.Slot))
	}
}

// ── Small helpers ────────────────────────────────────────────────────────────

func parseInt64(s string, def int64) int64 {
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	return def
}

// readAllLimited reads the request body up to max bytes, writing a 413/400 and
// returning an error on overflow / read failure.
func readAllLimited(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err == nil {
		return body, nil
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAuthError(w, http.StatusRequestEntityTooLarge, "save too large")
	} else {
		writeAuthError(w, http.StatusBadRequest, "read failed")
	}
	return nil, err
}
