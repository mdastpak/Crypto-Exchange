// Package marketdataadmin serves the admin routes of the global market data
// catalog (#270): the sources and venues that the workers collect, the
// assets and their links to listed currencies, the tiers and mapping of the
// pairs, the conflicts of identity, the jobs, and the history of their runs.
//
// Every change is audited in its own transaction (see marketdata.Store),
// and drops the cache of the public market data API on every instance, so
// that a hidden asset disappears at once.
package marketdataadmin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/pagination"

	"github.com/gin-gonic/gin"
)

// timeout bounds the work of one request.
const timeout = 10 * time.Second

// The error codes of the routes.
const (
	CodeNotFound         = "market_not_found"
	CodeAssetLinked      = "market_asset_linked"
	CodeAssetHidden      = "market_asset_hidden"
	CodeAssetNotLinked   = "market_asset_not_linked"
	CodeCurrencyNotFound = "market_currency_not_found"
	CodeCurrencyLinked   = "market_currency_linked"
	CodeConflictResolved = "market_conflict_resolved"
	CodeJobRunning       = "market_job_running"
)

// Handler serves the admin routes.
type Handler struct {
	store Store
	// invalidate drops the cache of the public API; nil when there is none.
	invalidate func(ctx context.Context) error
}

// New returns the handler. invalidate, when set, drops the cache of the
// public API after each change.
func New(store Store, invalidate func(ctx context.Context) error) *Handler {
	return &Handler{store: store, invalidate: invalidate}
}

// Register adds the routes under /market-data of g, which must check that
// the caller is an active administrator.
func (h *Handler) Register(g *gin.RouterGroup) {
	m := g.Group("/market-data")
	m.GET("/sources", h.ListSources)
	m.POST("/sources/:source/pause", h.PauseSource)
	m.POST("/sources/:source/resume", h.ResumeSource)
	m.GET("/venues", h.ListVenues)
	m.POST("/venues/:id/enable", h.EnableVenue)
	m.POST("/venues/:id/disable", h.DisableVenue)
	m.GET("/assets", h.ListAssets)
	m.GET("/assets/:id", h.GetAsset)
	m.POST("/assets/:id/hide", h.HideAsset)
	m.POST("/assets/:id/unhide", h.UnhideAsset)
	m.POST("/assets/:id/link", h.LinkAsset)
	m.POST("/assets/:id/unlink", h.UnlinkAsset)
	m.PUT("/assets/:id/tier", h.SetAssetTier)
	m.GET("/pairs", h.ListPairs)
	m.PUT("/pairs/:id/tier", h.SetPairTier)
	m.PUT("/pairs/:id/mapping", h.MapPair)
	m.GET("/conflicts", h.ListConflicts)
	m.POST("/conflicts/:id/resolve", h.ResolveConflict)
	m.GET("/jobs", h.ListJobs)
	m.POST("/jobs/:job/run", h.RunJob)
	m.GET("/runs", h.ListRuns)
}

// origin is the audit origin of a change by the administrator of the
// request.
func origin(c *gin.Context) audit.Event {
	e := audit.FromRequest(c)
	if claims, ok := auth.GetClaims(c); ok && claims.UUID != "" {
		e.ActorType, e.ActorID = audit.ActorAdmin, claims.UUID
	}
	return e
}

func validation(c *gin.Context, errs map[string]string) {
	helpers.SendError(c, http.StatusBadRequest, helpers.CodeValidationFailed, i18n.M("VALIDATION_FAILED"), gin.H{"errors": errs})
}

// sendError answers the errors of the store.
func sendError(c *gin.Context, err error, what string) {
	type answer struct {
		status  int
		code    string
		message i18n.Message
	}
	answers := []struct {
		err error
		answer
	}{
		{marketdata.ErrNotFound, answer{http.StatusNotFound, CodeNotFound, i18n.M("NOT_FOUND")}},
		{marketdata.ErrAssetLinked, answer{http.StatusConflict, CodeAssetLinked, i18n.M("MARKET_ASSET_LINKED")}},
		{marketdata.ErrAssetHidden, answer{http.StatusConflict, CodeAssetHidden, i18n.M("MARKET_ASSET_HIDDEN")}},
		{marketdata.ErrAssetNotLinked, answer{http.StatusConflict, CodeAssetNotLinked, i18n.M("MARKET_ASSET_NOT_LINKED")}},
		{marketdata.ErrCurrencyNotFound, answer{http.StatusUnprocessableEntity, CodeCurrencyNotFound, i18n.M("MARKET_CURRENCY_NOT_FOUND")}},
		{marketdata.ErrCurrencyLinked, answer{http.StatusConflict, CodeCurrencyLinked, i18n.M("MARKET_CURRENCY_LINKED")}},
		{marketdata.ErrConflictResolved, answer{http.StatusConflict, CodeConflictResolved, i18n.M("MARKET_CONFLICT_ALREADY_RESOLVED")}},
		{marketdata.ErrJobRunning, answer{http.StatusConflict, CodeJobRunning, i18n.M("MARKET_JOB_RUNNING")}},
	}
	for _, a := range answers {
		if errors.Is(err, a.err) {
			helpers.SendError(c, a.status, a.code, a.message, nil)
			return
		}
	}
	if errors.Is(err, marketdata.ErrInvalid) {
		validation(c, map[string]string{"request": strings.TrimPrefix(err.Error(), "marketdata: invalid value: ")})
		return
	}
	log.Printf("Market data admin: failed to %s: %v", what, err)
	helpers.SendResponse(c, http.StatusInternalServerError, i18n.M("INTERNAL_ERROR"), nil)
}

// changed drops the cache of the public API after a change. A failure is
// logged: the cached answers end with their time to live.
func (h *Handler) changed(ctx context.Context) {
	if h.invalidate == nil {
		return
	}
	if err := h.invalidate(ctx); err != nil {
		log.Printf("Market data admin: failed to drop the cache of the public API; it expires on its own: %v", err)
	}
}

// NoteRequest carries the reason of a change.
type NoteRequest struct {
	// Reason says why, for the audit log, at most 1000 characters.
	Reason string `json:"reason"`
}

// bind reads an optional JSON body into req; an empty body is an empty
// request.
func bind(c *gin.Context, req any) bool {
	if c.Request.ContentLength == 0 {
		return true
	}
	if err := c.ShouldBindJSON(req); err != nil {
		validation(c, map[string]string{"body": "must be a JSON object of the documented fields"})
		return false
	}
	return true
}

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		validation(c, map[string]string{"id": "must be a positive number"})
		return 0, false
	}
	return id, true
}

// page reads the page request and its position, an id.
func page(c *gin.Context) (pagination.Request, int64, bool) {
	r, errs := pagination.Parse(c)
	if errs != nil {
		validation(c, errs)
		return r, 0, false
	}
	var after int64
	if _, err := r.After(&after); err != nil {
		validation(c, map[string]string{"cursor": "is not a cursor of this list"})
		return r, 0, false
	}
	return r, after, true
}

// Views.

// SourceView is a source: its pause, its requests, and the state of its
// jobs.
type SourceView struct {
	Source string `json:"source"`
	Paused bool   `json:"paused"`
	// Reason is why it was paused.
	Reason    string     `json:"reason"`
	UpdatedAt *time.Time `json:"updated_at"`
	// RequestsThisMinute and RequestsThisMonth count the requests of every
	// worker in the current windows.
	RequestsThisMinute int `json:"requests_this_minute"`
	RequestsThisMonth  int `json:"requests_this_month"`
	// Healthy is false while a job of the source failed its last run.
	Healthy bool      `json:"healthy"`
	Jobs    []JobView `json:"jobs"`
}

// JobView is the state of a job.
type JobView struct {
	Job    string `json:"job"`
	Source string `json:"source"`
	// Running is true while a worker, Holder, runs it.
	Running bool   `json:"running"`
	Holder  string `json:"holder"`
	// Failures counts its failed runs in a row.
	Failures       int        `json:"failures"`
	NextRunAt      time.Time  `json:"next_run_at"`
	LastStartedAt  *time.Time `json:"last_started_at"`
	LastFinishedAt *time.Time `json:"last_finished_at"`
	LastSuccessAt  *time.Time `json:"last_success_at"`
}

func jobView(j marketdata.JobState) JobView {
	return JobView{Job: j.Job, Source: marketdata.SourceOf(j.Job), Running: j.Running, Holder: j.Holder,
		Failures: j.Failures, NextRunAt: j.NextRunAt, LastStartedAt: j.LastStartedAt,
		LastFinishedAt: j.LastFinishedAt, LastSuccessAt: j.LastSuccessAt}
}

// SourceList is the sources, by name.
type SourceList struct {
	Items []SourceView `json:"items"`
}

// JobList is the jobs, by name.
type JobList struct {
	Items []JobView `json:"items"`
}

// VenueView is a venue.
type VenueView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind" enums:"cex,dex"`
	Region  string `json:"region" enums:"global,ir"`
	Country string `json:"country"`
	Website string `json:"website"`
	Enabled bool   `json:"enabled"`
}

func venueView(v marketdata.Venue) VenueView {
	return VenueView{ID: v.ID, Name: v.Name, Kind: v.Kind, Region: v.Region, Country: v.Country, Website: v.Website, Enabled: v.Enabled}
}

// VenueList is the venues, by id.
type VenueList struct {
	Items []VenueView `json:"items"`
}

// AssetView is an asset as administrators see it.
type AssetView struct {
	ID     int64  `json:"id"`
	Slug   string `json:"slug"`
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	Kind   string `json:"kind" enums:"coin,token,unknown"`
	Status string `json:"status" enums:"unverified,hidden,linked"`
	// CurrencyHK is the listed currency of a linked asset.
	CurrencyHK    *string `json:"currency_hk"`
	Active        bool    `json:"active"`
	MarketCapRank *int    `json:"market_cap_rank"`
	// TierOverride is the administrators' tier of its pairs.
	TierOverride *string   `json:"tier_override" enums:"hot,active,other"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func assetView(a marketdata.AdminAsset) AssetView {
	return AssetView{ID: a.ID, Slug: a.Slug, Symbol: a.Symbol, Name: a.Name, Kind: a.Kind, Status: a.Status,
		CurrencyHK: a.CurrencyHK, Active: a.Active, MarketCapRank: a.MarketCapRank, TierOverride: a.TierOverride,
		UpdatedAt: a.UpdatedAt}
}

// AssetPage is a page of assets.
type AssetPage struct {
	Items      []AssetView `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// PairView is a pair as administrators see it.
type PairView struct {
	ID           int64  `json:"id"`
	Venue        string `json:"venue"`
	VenueSymbol  string `json:"venue_symbol"`
	BaseSymbol   string `json:"base_symbol"`
	QuoteSymbol  string `json:"quote_symbol"`
	BaseAssetID  *int64 `json:"base_asset_id"`
	QuoteAssetID *int64 `json:"quote_asset_id"`
	Active       bool   `json:"active"`
	// Tier is the tier that the tiers job gave it; TierOverride, when set,
	// wins over it.
	Tier         string  `json:"tier" enums:"hot,active,other"`
	TierOverride *string `json:"tier_override" enums:"hot,active,other"`
}

// PairPage is a page of pairs.
type PairPage struct {
	Items      []PairView `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// ConflictView is a conflict of identity.
type ConflictView struct {
	ID         int64  `json:"id"`
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
	Kind       string `json:"kind" enums:"contract,identity,symbol"`
	// Subject is what conflicts: network:address for a contract, the
	// symbol for a symbol.
	Subject string `json:"subject"`
	// AssetID is the asset that the catalog has; ClaimedBy, when known,
	// the asset that the source's record belongs to.
	AssetID      *int64     `json:"asset_id"`
	ClaimedBy    *int64     `json:"claimed_by"`
	Detail       string     `json:"detail"`
	CreatedAt    time.Time  `json:"created_at"`
	ResolvedAt   *time.Time `json:"resolved_at"`
	ResolvedNote string     `json:"resolved_note"`
}

// ConflictPage is a page of conflicts.
type ConflictPage struct {
	Items      []ConflictView `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// RunView is a run of a job.
type RunView struct {
	ID           int64      `json:"id"`
	Job          string     `json:"job"`
	Node         string     `json:"node"`
	Mode         string     `json:"mode" enums:"live,dry_run"`
	Status       string     `json:"status" enums:"running,succeeded,failed"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	ItemsRead    int        `json:"items_read"`
	ItemsWritten int        `json:"items_written"`
	Error        string     `json:"error"`
}

// RunPage is a page of runs.
type RunPage struct {
	Items      []RunView `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// newPage builds a page of views of items, fetched with r.Limit + 1.
func newPage[T, V any](items []T, r pagination.Request, id func(T) int64, view func(T) V) ([]V, string, error) {
	p, err := pagination.NewPage(items, r, func(t T) interface{} { return id(t) })
	if err != nil {
		return nil, "", err
	}
	views := make([]V, len(p.Items))
	for i, t := range p.Items {
		views[i] = view(t)
	}
	return views, p.NextCursor, nil
}

// Sources and jobs.

// ListSources lists the sources.
//
// @Summary List the market data sources
// @Description Every source that a worker has used, that has a job, or that an administrator paused, with its pause, its requests in the current minute and month across every worker, and the state of its jobs. A source is healthy while none of its jobs failed its last run.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=SourceList}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/sources [get]
func (h *Handler) ListSources(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	controls, err := h.store.SourceControls(ctx)
	if err != nil {
		sendError(c, err, "read the sources")
		return
	}
	usage, err := h.store.SourceUsages(ctx)
	if err != nil {
		sendError(c, err, "read the sources")
		return
	}
	jobs, err := h.store.JobStates(ctx)
	if err != nil {
		sendError(c, err, "read the sources")
		return
	}
	sources := map[string]*SourceView{}
	view := func(name string) *SourceView {
		if v, ok := sources[name]; ok {
			return v
		}
		v := &SourceView{Source: name, Healthy: true, Jobs: []JobView{}}
		sources[name] = v
		return v
	}
	for name, ctl := range controls {
		v := view(name)
		v.Paused, v.Reason, v.UpdatedAt = ctl.Paused, ctl.Reason, ctl.UpdatedAt
	}
	for name, u := range usage {
		v := view(name)
		v.RequestsThisMinute, v.RequestsThisMonth = u.Minute, u.Month
	}
	for _, j := range jobs {
		name := marketdata.SourceOf(j.Job)
		if name == "" {
			continue
		}
		v := view(name)
		v.Jobs = append(v.Jobs, jobView(j))
		if j.Failures > 0 {
			v.Healthy = false
		}
	}
	list := SourceList{Items: []SourceView{}}
	for _, v := range sources {
		list.Items = append(list.Items, *v)
	}
	slices.SortFunc(list.Items, func(a, b SourceView) int { return strings.Compare(a.Source, b.Source) })
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_SOURCES_RETRIEVED"), list)
}

// PauseSource pauses a source.
//
// @Summary Pause a market data source
// @Description Every worker stops claiming the jobs that call the source, such as catalog:coingecko or tickers:binance, and a run in progress stops at its next batch of writes. The jobs stay due and run once the source is resumed. The reason is required. Pausing a paused source changes nothing. Audited as market_data.source_pause.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param source path string true "The source, such as coingecko or binance"
// @Param request body NoteRequest true "Why"
// @Success 200 {object} models.APIResponse{info=SourceView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/sources/{source}/pause [post]
func (h *Handler) PauseSource(c *gin.Context) { h.setSourcePaused(c, true) }

// ResumeSource resumes a source.
//
// @Summary Resume a market data source
// @Description The workers claim its jobs again at their next poll. Audited as market_data.source_resume.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param source path string true "The source, such as coingecko or binance"
// @Param request body NoteRequest false "Why"
// @Success 200 {object} models.APIResponse{info=SourceView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/sources/{source}/resume [post]
func (h *Handler) ResumeSource(c *gin.Context) { h.setSourcePaused(c, false) }

func (h *Handler) setSourcePaused(c *gin.Context, paused bool) {
	var req NoteRequest
	if !bind(c, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	ctl, err := h.store.SetSourcePaused(ctx, c.Param("source"), paused, req.Reason, origin(c))
	if err != nil {
		sendError(c, err, "change the source")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_SOURCE_UPDATED"),
		SourceView{Source: ctl.Source, Paused: ctl.Paused, Reason: ctl.Reason, UpdatedAt: ctl.UpdatedAt, Healthy: true, Jobs: []JobView{}})
}

// ListJobs lists the jobs.
//
// @Summary List the market data jobs
// @Description Every job that a worker has claimed, by name, with the worker that runs it now, its failures in a row, and when it runs next.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=JobList}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/jobs [get]
func (h *Handler) ListJobs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	jobs, err := h.store.JobStates(ctx)
	if err != nil {
		sendError(c, err, "read the jobs")
		return
	}
	list := JobList{Items: make([]JobView, len(jobs))}
	for i, j := range jobs {
		list.Items[i] = jobView(j)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_JOBS_RETRIEVED"), list)
}

// RunJob runs a job now.
//
// @Summary Run a market data job now
// @Description Makes the job due now: the first worker that polls and reaches its sources runs it, within market_data.poll_seconds. A job that a worker runs now is refused with 409 market_job_running. Audited as market_data.job_run.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Param job path string true "The job, such as tickers:binance"
// @Success 202 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/jobs/{job}/run [post]
func (h *Handler) RunJob(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	if err := h.store.RunJobNow(ctx, c.Param("job"), origin(c)); err != nil {
		sendError(c, err, "run the job")
		return
	}
	helpers.SendResponse(c, http.StatusAccepted, i18n.M("MARKET_JOB_DUE"), nil)
}

// ListRuns lists the runs of the jobs.
//
// @Summary List the runs of the market data jobs
// @Description The runs, newest first, a page at a time, with what each read and wrote and its error.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Param job query string false "The runs of one job, such as tickers:binance"
// @Param status query string false "The status" Enums(running, succeeded, failed)
// @Param limit query int false "The number of runs, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=RunPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/runs [get]
func (h *Handler) ListRuns(c *gin.Context) {
	r, before, ok := page(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	runs, err := h.store.ListRuns(ctx, marketdata.RunFilter{Job: c.Query("job"), Status: c.Query("status"), BeforeID: before, Limit: r.Limit + 1})
	if err != nil {
		sendError(c, err, "read the runs")
		return
	}
	items, next, err := newPage(runs, r, func(run marketdata.Run) int64 { return run.ID }, func(run marketdata.Run) RunView {
		return RunView{ID: run.ID, Job: run.Job, Node: run.Node, Mode: run.Mode, Status: run.Status, StartedAt: run.StartedAt,
			FinishedAt: run.FinishedAt, ItemsRead: run.ItemsRead, ItemsWritten: run.ItemsWritten, Error: run.Error}
	})
	if err != nil {
		sendError(c, err, "read the runs")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_RUNS_RETRIEVED"), RunPage{Items: items, NextCursor: next})
}

// Venues.

// ListVenues lists the venues.
//
// @Summary List the market data venues
// @Description Every exchange that the catalog knows, by id, enabled or not.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Success 200 {object} models.APIResponse{info=VenueList}
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/venues [get]
func (h *Handler) ListVenues(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	venues, err := h.store.Venues(ctx)
	if err != nil {
		sendError(c, err, "read the venues")
		return
	}
	list := VenueList{Items: make([]VenueView, len(venues))}
	for i, v := range venues {
		list.Items[i] = venueView(v)
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_VENUES_RETRIEVED"), list)
}

// EnableVenue enables a venue.
//
// @Summary Enable a market data venue
// @Description The jobs of the venue collect it again, and the public API shows it. Audited as market_data.venue_enable.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The venue, such as binance"
// @Param request body NoteRequest false "Why"
// @Success 200 {object} models.APIResponse{info=VenueView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/venues/{id}/enable [post]
func (h *Handler) EnableVenue(c *gin.Context) { h.setVenueEnabled(c, true) }

// DisableVenue disables a venue.
//
// @Summary Disable a market data venue
// @Description Its jobs collect nothing, and the public API leaves it out at once. Its data is kept. Audited as market_data.venue_disable.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "The venue, such as binance"
// @Param request body NoteRequest false "Why"
// @Success 200 {object} models.APIResponse{info=VenueView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/venues/{id}/disable [post]
func (h *Handler) DisableVenue(c *gin.Context) { h.setVenueEnabled(c, false) }

func (h *Handler) setVenueEnabled(c *gin.Context, enabled bool) {
	var req NoteRequest
	if !bind(c, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	v, err := h.store.SetVenueEnabled(ctx, c.Param("id"), enabled, req.Reason, origin(c))
	if err != nil {
		sendError(c, err, "change the venue")
		return
	}
	h.changed(ctx)
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_VENUE_UPDATED"), venueView(v))
}

// Assets.

// ListAssets lists the assets.
//
// @Summary List the assets of the market data catalog
// @Description Every asset, hidden ones included, in the order they were added, a page at a time.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Param status query string false "The status" Enums(unverified, hidden, linked)
// @Param q query string false "A symbol, or the start of a slug or name"
// @Param limit query int false "The number of assets, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=AssetPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets [get]
func (h *Handler) ListAssets(c *gin.Context) {
	r, after, ok := page(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	assets, err := h.store.AdminAssets(ctx, marketdata.AssetFilter{Status: c.Query("status"), Query: c.Query("q"), AfterID: after, Limit: r.Limit + 1})
	if err != nil {
		sendError(c, err, "read the assets")
		return
	}
	items, next, err := newPage(assets, r, func(a marketdata.AdminAsset) int64 { return a.ID }, assetView)
	if err != nil {
		sendError(c, err, "read the assets")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_ASSETS_RETRIEVED"), AssetPage{Items: items, NextCursor: next})
}

// GetAsset returns an asset.
//
// @Summary Show an asset of the market data catalog
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Param id path int true "The asset"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets/{id} [get]
func (h *Handler) GetAsset(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	a, err := h.store.AdminAsset(ctx, id)
	if err != nil {
		sendError(c, err, "read the asset")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_ASSET_RETRIEVED"), assetView(a))
}

// assetChange runs a change of an asset and answers with the asset.
func (h *Handler) assetChange(c *gin.Context, req any, change func(ctx context.Context, id int64) (marketdata.AdminAsset, error)) {
	id, ok := idParam(c)
	if !ok || !bind(c, req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	a, err := change(ctx, id)
	if err != nil {
		sendError(c, err, "change the asset")
		return
	}
	h.changed(ctx)
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_ASSET_UPDATED"), assetView(a))
}

// HideAsset hides an asset.
//
// @Summary Hide an asset of the market data catalog
// @Description The public API never serves it again, at once on every instance; the jobs keep collecting it. The reason is required. A linked asset is refused with 409 market_asset_linked. Audited as market_data.asset_hide.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The asset"
// @Param request body NoteRequest true "Why"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets/{id}/hide [post]
func (h *Handler) HideAsset(c *gin.Context) {
	var req NoteRequest
	h.assetChange(c, &req, func(ctx context.Context, id int64) (marketdata.AdminAsset, error) {
		return h.store.SetAssetHidden(ctx, id, true, req.Reason, origin(c))
	})
}

// UnhideAsset shows a hidden asset again.
//
// @Summary Show a hidden asset again
// @Description The asset is unverified again. Audited as market_data.asset_unhide.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The asset"
// @Param request body NoteRequest false "Why"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets/{id}/unhide [post]
func (h *Handler) UnhideAsset(c *gin.Context) {
	var req NoteRequest
	h.assetChange(c, &req, func(ctx context.Context, id int64) (marketdata.AdminAsset, error) {
		return h.store.SetAssetHidden(ctx, id, false, req.Reason, origin(c))
	})
}

// LinkRequest links an asset to a listed currency.
type LinkRequest struct {
	// CurrencyHK is the hk of the listed currency.
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	// Reason says why, at most 1000 characters.
	Reason string `json:"reason"`
}

// LinkAsset links an asset to a listed currency.
//
// @Summary Link an asset to a listed currency
// @Description The asset is then labeled verified, as the same asset as the currency. The link only reads the currency, and never changes it. A currency is linked to one asset at most (409 market_currency_linked); an asset linked to another currency must be unlinked first (409 market_asset_linked), and a hidden asset shown first (409 market_asset_hidden). A currency that does not exist, or was deleted, answers 422 market_currency_not_found. Audited as market_data.asset_link.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The asset"
// @Param request body LinkRequest true "The currency"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure 422 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets/{id}/link [post]
func (h *Handler) LinkAsset(c *gin.Context) {
	var req LinkRequest
	h.assetChange(c, &req, func(ctx context.Context, id int64) (marketdata.AdminAsset, error) {
		return h.store.LinkAsset(ctx, id, req.CurrencyHK, req.Reason, origin(c))
	})
}

// UnlinkAsset ends the link of an asset.
//
// @Summary Unlink an asset from its listed currency
// @Description The asset is unverified again; the currency is not changed. An asset that is not linked answers 409 market_asset_not_linked. Audited as market_data.asset_unlink.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The asset"
// @Param request body NoteRequest false "Why"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets/{id}/unlink [post]
func (h *Handler) UnlinkAsset(c *gin.Context) {
	var req NoteRequest
	h.assetChange(c, &req, func(ctx context.Context, id int64) (marketdata.AdminAsset, error) {
		return h.store.UnlinkAsset(ctx, id, req.Reason, origin(c))
	})
}

// TierRequest sets or clears a tier.
type TierRequest struct {
	// Tier is hot, active, or other; null clears the administrators' tier,
	// and the tiers job decides again.
	Tier *string `json:"tier" enums:"hot,active,other"`
}

// SetAssetTier sets the tier of the pairs of an asset.
//
// @Summary Set the collection tier of an asset
// @Description Sets the tier of every pair of the asset, or clears it with null; a tier set on a pair wins over it. The tiers job applies it at its next run. Audited as market_data.tier_override.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The asset"
// @Param request body TierRequest true "The tier"
// @Success 200 {object} models.APIResponse{info=AssetView}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/assets/{id}/tier [put]
func (h *Handler) SetAssetTier(c *gin.Context) {
	var req TierRequest
	h.assetChange(c, &req, func(ctx context.Context, id int64) (marketdata.AdminAsset, error) {
		if err := h.store.SetAssetTierOverride(ctx, id, req.Tier, origin(c)); err != nil {
			return marketdata.AdminAsset{}, err
		}
		return h.store.AdminAsset(ctx, id)
	})
}

// Pairs.

func pairView(p marketdata.AdminPair) PairView {
	return PairView{ID: p.ID, Venue: p.VenueID, VenueSymbol: p.VenueSymbol, BaseSymbol: p.BaseSymbol, QuoteSymbol: p.QuoteSymbol,
		BaseAssetID: p.BaseAssetID, QuoteAssetID: p.QuoteAssetID, Active: p.Active, Tier: p.Tier, TierOverride: p.TierOverride}
}

// ListPairs lists the pairs.
//
// @Summary List the pairs of the market data venues
// @Description The pairs, in the order they were added, a page at a time. unmapped=true selects the pairs that the jobs could not map to the assets of the catalog, which the comparison leaves out.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Param venue query string false "The pairs of one venue, such as binance"
// @Param unmapped query bool false "Only the pairs without a base or quote asset"
// @Param limit query int false "The number of pairs, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=PairPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/pairs [get]
func (h *Handler) ListPairs(c *gin.Context) {
	r, after, ok := page(c)
	if !ok {
		return
	}
	unmapped := false
	if raw := c.Query("unmapped"); raw != "" {
		var err error
		if unmapped, err = strconv.ParseBool(raw); err != nil {
			validation(c, map[string]string{"unmapped": "must be true or false"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	pairs, err := h.store.AdminPairs(ctx, marketdata.PairFilter{Venue: c.Query("venue"), Unmapped: unmapped, AfterID: after, Limit: r.Limit + 1})
	if err != nil {
		sendError(c, err, "read the pairs")
		return
	}
	items, next, err := newPage(pairs, r, func(p marketdata.AdminPair) int64 { return p.ID }, pairView)
	if err != nil {
		sendError(c, err, "read the pairs")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_PAIRS_RETRIEVED"), PairPage{Items: items, NextCursor: next})
}

// SetPairTier sets the tier of a pair.
//
// @Summary Set the collection tier of a pair
// @Description Sets the tier of the pair, or clears it with null; it wins over the tier of its asset. The tiers job applies it at its next run. Audited as market_data.tier_override.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The pair"
// @Param request body TierRequest true "The tier"
// @Success 200 {object} models.APIResponse
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/pairs/{id}/tier [put]
func (h *Handler) SetPairTier(c *gin.Context) {
	var req TierRequest
	h.pairChange(c, &req, func(ctx context.Context, id int64) error {
		return h.store.SetPairTierOverride(ctx, id, req.Tier, origin(c))
	})
}

// MappingRequest maps a pair to the assets of the catalog.
type MappingRequest struct {
	BaseAssetID  int64 `json:"base_asset_id"`
	QuoteAssetID int64 `json:"quote_asset_id"`
}

// MapPair maps a pair.
//
// @Summary Map a pair to the assets of the catalog
// @Description Sets the base and quote assets of a pair that the jobs could not map, or mapped wrongly. The jobs keep the mapping. Audited as market_data.pair_map.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The pair"
// @Param request body MappingRequest true "The assets"
// @Success 200 {object} models.APIResponse
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/pairs/{id}/mapping [put]
func (h *Handler) MapPair(c *gin.Context) {
	var req MappingRequest
	h.pairChange(c, &req, func(ctx context.Context, id int64) error {
		if req.BaseAssetID <= 0 || req.QuoteAssetID <= 0 {
			return fmt.Errorf("%w: base_asset_id and quote_asset_id are required", marketdata.ErrInvalid)
		}
		return h.store.MapPair(ctx, id, req.BaseAssetID, req.QuoteAssetID, origin(c))
	})
}

func (h *Handler) pairChange(c *gin.Context, req any, change func(ctx context.Context, id int64) error) {
	id, ok := idParam(c)
	if !ok || !bind(c, req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	if err := change(ctx, id); err != nil {
		sendError(c, err, "change the pair")
		return
	}
	h.changed(ctx)
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_PAIR_UPDATED"), nil)
}

// Conflicts.

// ListConflicts lists the conflicts of identity.
//
// @Summary List the conflicts of identity of the catalog
// @Description What the catalog sync could not decide alone, oldest first, a page at a time: the open conflicts, or the resolved ones with resolved=true.
// @Tags admin market data
// @Produce json
// @Security BearerAuth
// @Param source query string false "The conflicts of one source, such as coinmarketcap"
// @Param resolved query bool false "The resolved conflicts instead of the open ones"
// @Param limit query int false "The number of conflicts, 50 by default" minimum(1) maximum(200)
// @Param cursor query string false "The next_cursor of the previous page"
// @Success 200 {object} models.APIResponse{info=ConflictPage}
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/conflicts [get]
func (h *Handler) ListConflicts(c *gin.Context) {
	r, after, ok := page(c)
	if !ok {
		return
	}
	resolved := false
	if raw := c.Query("resolved"); raw != "" {
		var err error
		if resolved, err = strconv.ParseBool(raw); err != nil {
			validation(c, map[string]string{"resolved": "must be true or false"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	conflicts, err := h.store.ListConflicts(ctx, marketdata.ConflictFilter{Source: c.Query("source"), Resolved: resolved, AfterID: after, Limit: r.Limit + 1})
	if err != nil {
		sendError(c, err, "read the conflicts")
		return
	}
	items, next, err := newPage(conflicts, r, func(x marketdata.AdminConflict) int64 { return x.ID }, func(x marketdata.AdminConflict) ConflictView {
		return ConflictView{ID: x.ID, Source: x.Source, ExternalID: x.ExternalID, Kind: x.Kind, Subject: x.Subject, AssetID: x.AssetID,
			ClaimedBy: x.ClaimedBy, Detail: x.Detail, CreatedAt: x.CreatedAt, ResolvedAt: x.ResolvedAt, ResolvedNote: x.ResolvedNote}
	})
	if err != nil {
		sendError(c, err, "read the conflicts")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_CONFLICTS_RETRIEVED"), ConflictPage{Items: items, NextCursor: next})
}

// ResolveRequest resolves a conflict.
type ResolveRequest struct {
	// Note says what was decided, at most 1000 characters.
	Note string `json:"note"`
}

// ResolveConflict resolves a conflict.
//
// @Summary Resolve a conflict of identity
// @Description Closes the conflict with a note of what was decided. The fix itself, such as hiding an asset or mapping a pair, is its own change. The note is required; a resolved conflict answers 409 market_conflict_resolved. A conflict that the sync meets again later opens anew. Audited as market_data.conflict_resolve.
// @Tags admin market data
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "The conflict"
// @Param request body ResolveRequest true "The decision"
// @Success 200 {object} models.APIResponse
// @Failure 400 {object} models.APIResponse
// @Failure 401 {object} models.APIResponse
// @Failure 403 {object} models.APIResponse
// @Failure 404 {object} models.APIResponse
// @Failure 409 {object} models.APIResponse
// @Failure default {object} models.APIResponse "Any other error, in the envelope with a code"
// @Router /api/v1/admin/market-data/conflicts/{id}/resolve [post]
func (h *Handler) ResolveConflict(c *gin.Context) {
	id, ok := idParam(c)
	var req ResolveRequest
	if !ok || !bind(c, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	if err := h.store.ResolveConflict(ctx, id, req.Note, origin(c)); err != nil {
		sendError(c, err, "resolve the conflict")
		return
	}
	helpers.SendResponse(c, http.StatusOK, i18n.M("MARKET_CONFLICT_RESOLVED"), nil)
}
