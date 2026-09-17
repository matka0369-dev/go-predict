package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"predictsim/prediction-service/internal/auth"
	"predictsim/prediction-service/internal/db"
)

// maxImageBytes/imageDataURLRe/allowedImageMime mirror core-service's
// image-data-url.util.ts exactly — same 2MB decoded cap, same PNG/JPEG/WebP
// allowlist, so an image this service accepts is one core-service's
// reviewer-side (still Node) would also have accepted, and vice versa.
const maxImageBytes = 2 * 1024 * 1024

var imageDataURLRe = regexp.MustCompile(`^data:(image/(?:png|jpeg|webp));base64,([A-Za-z0-9+/]+=*)$`)

func parseImageDataURL(value string) (mimeType string, data []byte, ok bool) {
	m := imageDataURLRe.FindStringSubmatch(value)
	if m == nil {
		return "", nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil || len(decoded) == 0 || len(decoded) > maxImageBytes {
		return "", nil, false
	}
	return m[1], decoded, true
}

const maxGrant = 10_000_000

type createTokenRequestBody struct {
	Kind   string  `json:"kind"`
	Amount int     `json:"amount"`
	Note   *string `json:"note"`
	Image  *string `json:"image"`
}

type tokenRequestUserRef struct {
	ID       string  `json:"id"`
	Username string  `json:"username"`
	AgentID  *string `json:"agentId"`
}

type tokenRequestActorRef struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type tokenRequestResponse struct {
	ID             string                `json:"id"`
	Kind           string                `json:"kind"`
	Status         string                `json:"status"`
	Amount         int                   `json:"amount"`
	Note           *string               `json:"note"`
	ImageMimeType  *string               `json:"imageMimeType"`
	ClaimedAt      *string               `json:"claimedAt"`
	ResolvedAt     *string               `json:"resolvedAt"`
	ResolutionNote *string               `json:"resolutionNote"`
	CreatedAt      string                `json:"createdAt"`
	Requester      tokenRequestUserRef   `json:"requester"`
	ClaimedBy      *tokenRequestActorRef `json:"claimedBy"`
	ResolvedBy     *tokenRequestActorRef `json:"resolvedBy"`
	LedgerEntryID  *string               `json:"ledgerEntryId"`
}

// CreateTokenRequest mirrors RequestsService.create — one open request per
// kind at a time, a SURRENDER precheck against both wallets (a courtesy;
// the authoritative check happens at approval, still Node's job), and the
// same image validation the DTO + service double-check on the Node side.
func CreateTokenRequest(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "Only a Player may raise a token request", http.StatusForbidden)
			return
		}

		var body createTokenRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if body.Kind != "TOP_UP" && body.Kind != "SURRENDER" {
			http.Error(w, "kind must be TOP_UP or SURRENDER", http.StatusBadRequest)
			return
		}
		if body.Amount < 1 || body.Amount > maxGrant {
			http.Error(w, "Invalid amount", http.StatusBadRequest)
			return
		}
		if body.Note != nil && len(*body.Note) > 500 {
			http.Error(w, "note is too long", http.StatusBadRequest)
			return
		}

		var imageMimeType *string
		var imageData []byte
		if body.Image != nil {
			mt, data, ok := parseImageDataURL(*body.Image)
			if !ok {
				http.Error(w, "image must be a PNG, JPEG, or WebP data URL of 2MB or less", http.StatusBadRequest)
				return
			}
			imageMimeType, imageData = &mt, data
		}

		var agentID *string
		var balance, winningsBalance int
		err := pool.QueryRow(r.Context(), `
			SELECT agent_id, balance, winnings_balance FROM users WHERE id = $1
		`, user.ID).Scan(&agentID, &balance, &winningsBalance)
		if err != nil {
			http.Error(w, "Failed to load account", http.StatusInternalServerError)
			return
		}
		if agentID == nil {
			http.Error(w, "Your account has no agent", http.StatusBadRequest)
			return
		}

		if body.Kind == "SURRENDER" {
			held := balance + winningsBalance
			if body.Amount > held {
				http.Error(w, "You only hold "+strconv.Itoa(held)+" tokens", http.StatusBadRequest)
				return
			}
		}

		var openCount int
		err = pool.QueryRow(r.Context(), `
			SELECT count(*) FROM token_requests WHERE requester_id = $1 AND kind = $2::token_request_kind AND status = 'PENDING'
		`, user.ID, body.Kind).Scan(&openCount)
		if err != nil {
			http.Error(w, "Failed to check pending requests", http.StatusInternalServerError)
			return
		}
		if openCount > 0 {
			http.Error(w, "You already have a pending "+body.Kind+" request", http.StatusConflict)
			return
		}

		id := db.NewUUID()
		_, err = pool.Exec(r.Context(), `
			INSERT INTO token_requests (id, requester_id, kind, amount, note, image_data, image_mime_type, updated_at)
			VALUES ($1, $2, $3::token_request_kind, $4, $5, $6, $7, now())
		`, id, user.ID, body.Kind, body.Amount, body.Note, imageData, imageMimeType)
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}

		req, err := loadTokenRequest(r, pool, id)
		if err != nil {
			http.Error(w, "Failed to load created request", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, req)
	}
}

// MyTokenRequests mirrors RequestsService.mine.
func MyTokenRequests(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "Only a Player has its own token requests", http.StatusForbidden)
			return
		}

		rows, err := pool.Query(r.Context(), tokenRequestSelectSQL+`
			WHERE tr.requester_id = $1
			ORDER BY tr.created_at DESC
		`, user.ID)
		if err != nil {
			http.Error(w, "Failed to load requests", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		out := []tokenRequestResponse{}
		for rows.Next() {
			req, err := scanTokenRequest(rows)
			if err != nil {
				http.Error(w, "Failed to load requests", http.StatusInternalServerError)
				return
			}
			out = append(out, req)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "Failed to load requests", http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, out)
	}
}

// CancelTokenRequest mirrors RequestsService.cancel — a conditional update
// on `status = PENDING` (0 rows ⇒ 409), same as the Node side, so this and
// Node's approve/reject can race safely against each other on the same row.
func CancelTokenRequest(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		if user.AccountType != "PLAYER" {
			http.Error(w, "Only a Player may cancel its own request", http.StatusForbidden)
			return
		}
		id := chi.URLParam(r, "id")

		tag, err := pool.Exec(r.Context(), `
			UPDATE token_requests SET status = 'CANCELLED', updated_at = now()
			WHERE id = $1 AND requester_id = $2 AND status = 'PENDING'
		`, id, user.ID)
		if err != nil {
			http.Error(w, "Failed to cancel request", http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 {
			http.Error(w, "That request is not yours, or is no longer pending", http.StatusConflict)
			return
		}

		req, err := loadTokenRequest(r, pool, id)
		if err != nil {
			http.Error(w, "Failed to load cancelled request", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, req)
	}
}

// GetTokenRequestImage mirrors RequestsService.getImage's owner branch —
// this service's only caller is the Player portal, so the reviewer
// (Agent/Admin) branch stays Node-only. 404, not 403, on any denial —
// existence isn't information the caller is owed, same convention Node
// uses throughout.
func GetTokenRequestImage(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := auth.FromContext(r.Context())
		id := chi.URLParam(r, "id")

		var requesterID string
		var imageData []byte
		var imageMimeType *string
		err := pool.QueryRow(r.Context(), `
			SELECT requester_id, image_data, image_mime_type FROM token_requests WHERE id = $1
		`, id).Scan(&requesterID, &imageData, &imageMimeType)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "Request or image not found", http.StatusNotFound)
				return
			}
			http.Error(w, "Failed to load image", http.StatusInternalServerError)
			return
		}
		if len(imageData) == 0 || imageMimeType == nil || requesterID != user.ID {
			http.Error(w, "Request or image not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", *imageMimeType)
		w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(imageData)
	}
}

const tokenRequestSelectSQL = `
	SELECT tr.id, tr.kind, tr.status, tr.amount, tr.note, tr.image_mime_type,
	       tr.claimed_at, tr.resolved_at, tr.resolution_note, tr.created_at, tr.ledger_entry_id,
	       req.id, req.username, req.agent_id,
	       cb.id, cb.username,
	       rb.id, rb.username
	FROM token_requests tr
	JOIN users req ON req.id = tr.requester_id
	LEFT JOIN users cb ON cb.id = tr.claimed_by_id
	LEFT JOIN users rb ON rb.id = tr.resolved_by_id
`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTokenRequest(row rowScanner) (tokenRequestResponse, error) {
	var (
		res                              tokenRequestResponse
		claimedAt, resolvedAt            *time.Time
		createdAt                        time.Time
		claimedByID, claimedByUsername   *string
		resolvedByID, resolvedByUsername *string
	)
	err := row.Scan(
		&res.ID, &res.Kind, &res.Status, &res.Amount, &res.Note, &res.ImageMimeType,
		&claimedAt, &resolvedAt, &res.ResolutionNote, &createdAt, &res.LedgerEntryID,
		&res.Requester.ID, &res.Requester.Username, &res.Requester.AgentID,
		&claimedByID, &claimedByUsername,
		&resolvedByID, &resolvedByUsername,
	)
	if err != nil {
		return res, err
	}
	res.CreatedAt = createdAt.Format(time.RFC3339)
	if claimedAt != nil {
		s := claimedAt.Format(time.RFC3339)
		res.ClaimedAt = &s
	}
	if resolvedAt != nil {
		s := resolvedAt.Format(time.RFC3339)
		res.ResolvedAt = &s
	}
	if claimedByID != nil {
		res.ClaimedBy = &tokenRequestActorRef{ID: *claimedByID, Username: *claimedByUsername}
	}
	if resolvedByID != nil {
		res.ResolvedBy = &tokenRequestActorRef{ID: *resolvedByID, Username: *resolvedByUsername}
	}
	return res, nil
}

func loadTokenRequest(r *http.Request, pool *pgxpool.Pool, id string) (tokenRequestResponse, error) {
	row := pool.QueryRow(r.Context(), tokenRequestSelectSQL+` WHERE tr.id = $1`, id)
	return scanTokenRequest(row)
}
