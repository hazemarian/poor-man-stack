package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// BackupTrigger is the same shape as deploy.BackupTrigger; redeclared here
// so the API package doesn't need to import deploy.
type BackupTrigger interface {
	Trigger(ctx context.Context) ([]string, error)
}

// BackupsHandler bundles deps for the /api/backups routes. Mount it under
// the /api group so Bearer auth applies.
type BackupsHandler struct {
	Store   *store.Store
	Trigger BackupTrigger // optional — POST /api/backups returns 503 when nil
}

// Mount wires GET /api/backups, POST /api/backups, GET /api/stacks/{name}/backups.
func (h *BackupsHandler) Mount(r chi.Router) {
	r.Get("/backups", h.list)
	r.Post("/backups", h.create)
	// stack-scoped listing lives on the existing stacks tree; mount via Mount on caller side.
}

// MountStackScoped wires the /stacks/{name}/backups route. Callers attach
// this to the same group that mounts /stacks so the URL shape is stable.
func (h *BackupsHandler) MountStackScoped(r chi.Router) {
	r.Get("/stacks/{name}/backups", h.listForStack)
}

type backupDTO struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	StackName     string `json:"stack_name,omitempty"`
	Revision      int64  `json:"revision,omitempty"`
	ArchivePaths  []string `json:"archive_paths,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
}

func toDTO(b *store.Backup) backupDTO {
	d := backupDTO{
		ID:           b.ID,
		Status:       b.Status,
		ErrorMessage: b.ErrorMessage,
		StartedAt:    b.StartedAt,
	}
	if b.StackName.Valid {
		d.StackName = b.StackName.String
	}
	if b.Revision.Valid {
		d.Revision = b.Revision.Int64
	}
	if b.FinishedAt.Valid {
		d.FinishedAt = b.FinishedAt.Int64
	}
	if b.ArchivePaths != "" {
		d.ArchivePaths = splitArchivePaths(b.ArchivePaths)
	}
	return d
}

func splitArchivePaths(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (h *BackupsHandler) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.Store.ListBackups(r.Context(), limit)
	if err != nil {
		writeBackupErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]backupDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, toDTO(b))
	}
	writeBackupJSON(w, http.StatusOK, map[string]any{"backups": out})
}

func (h *BackupsHandler) listForStack(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	rows, err := h.Store.ListBackupsForStack(r.Context(), name)
	if err != nil {
		writeBackupErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]backupDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, toDTO(b))
	}
	writeBackupJSON(w, http.StatusOK, map[string]any{"backups": out})
}

func (h *BackupsHandler) create(w http.ResponseWriter, r *http.Request) {
	if h.Trigger == nil {
		writeBackupErr(w, http.StatusServiceUnavailable, "backup trigger not configured")
		return
	}
	id, err := h.Store.CreateBackup(r.Context(), "", 0)
	if err != nil {
		writeBackupErr(w, http.StatusInternalServerError, "record backup: "+err.Error())
		return
	}
	paths, err := h.Trigger.Trigger(r.Context())
	if err != nil {
		_ = h.Store.FinishBackup(r.Context(), id, "failed", strings.Join(paths, ","), err.Error())
		writeBackupErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.Store.FinishBackup(r.Context(), id, "succeeded", strings.Join(paths, ","), ""); err != nil {
		writeBackupErr(w, http.StatusInternalServerError, "record finish: "+err.Error())
		return
	}
	writeBackupJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"status":        "succeeded",
		"archive_paths": paths,
	})
}

func writeBackupJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeBackupErr(w http.ResponseWriter, status int, msg string) {
	writeBackupJSON(w, status, map[string]string{"error": msg})
}
