package admin

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type containerUpdateService interface {
	ContainerMode() bool
	ContainerStart(context.Context, kanoupdate.StartRequest) (*kanoupdate.Session, error)
	ContainerCurrent(context.Context) (*kanoupdate.Session, error)
}

func (h *SystemHandler) containerUpdater() (containerUpdateService, bool) {
	updater, ok := h.updateSvc.(containerUpdateService)
	return updater, ok && updater.ContainerMode()
}

func (h *SystemHandler) performContainerUpdate(c *gin.Context, updater containerUpdateService) {
	performContainerAction(c, updater.ContainerStart)
}

func performContainerAction(c *gin.Context, start func(context.Context, kanoupdate.StartRequest) (*kanoupdate.Session, error)) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Error(c, http.StatusUnauthorized, "Administrator identity is required")
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		response.Error(c, http.StatusBadRequest, "Idempotency-Key is required for a container upgrade")
		return
	}
	var request struct {
		Version            string     `json:"version"`
		ExpectedDigest     string     `json:"expected_digest"`
		ExpectedScheduleAt *time.Time `json:"expected_scheduled_at"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "Invalid upgrade request")
		return
	}
	session, err := start(c.Request.Context(), kanoupdate.StartRequest{Version: request.Version, ExpectedDigest: request.ExpectedDigest, IdempotencyKey: key, Actor: "admin:" + strconv.FormatInt(subject.UserID, 10), ExpectedScheduleAt: request.ExpectedScheduleAt})
	if err != nil {
		containerUpdateError(c, err)
		return
	}
	if !progressCookie(c, session) {
		return
	}
	response.Accepted(c, gin.H{"operation_id": session.Job.ID, "progress_url": session.ProgressURL, "job": session.Job, "message": "Upgrade accepted; follow the independent progress page"})
}

// CurrentUpdate also refreshes the narrowly scoped cookie, allowing admins to
// reopen progress after refreshing or reconnecting to the application.
func (h *SystemHandler) CurrentUpdate(c *gin.Context) {
	updater, ok := h.containerUpdater()
	if !ok {
		response.ErrorFrom(c, service.ErrForkUpdateManaged)
		return
	}
	session, err := updater.ContainerCurrent(c.Request.Context())
	if err != nil {
		containerUpdateError(c, err)
		return
	}
	if session == nil || session.Job == nil {
		response.Success(c, gin.H{"job": nil})
		return
	}
	if !progressCookie(c, session) {
		return
	}
	response.Success(c, gin.H{"job": session.Job, "operation_id": session.Job.ID, "progress_url": session.ProgressURL})
}

func progressCookie(c *gin.Context, session *kanoupdate.Session) bool {
	if session == nil || session.Job == nil || len(session.Job.ID) != 35 || !strings.HasPrefix(session.Job.ID, "up-") || session.ProgressURL != "/kano-updates/"+session.Job.ID || len(session.Capability) != 64 {
		response.Error(c, http.StatusServiceUnavailable, "Updater progress is unavailable")
		return false
	}
	if _, err := hex.DecodeString(session.Job.ID[3:]); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Updater progress is unavailable")
		return false
	}
	if _, err := hex.DecodeString(session.Capability); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "Updater progress is unavailable")
		return false
	}
	c.Header("Cache-Control", "no-store")
	maxAge := 24 * 60 * 60
	if !session.ExpiresAt.IsZero() {
		seconds := int(time.Until(session.ExpiresAt).Seconds())
		if seconds > 0 && seconds <= 48*60*60 {
			maxAge = seconds
		}
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: kanoupdate.CookieName(session.Job.ID), Value: session.Capability, Path: session.ProgressURL, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
	return true
}

func containerUpdateError(c *gin.Context, err error) {
	var remote *kanoupdate.RemoteError
	if errors.As(err, &remote) {
		status := remote.Status
		if status != 400 && status != 403 && status != 409 && status != 503 {
			status = 503
		}
		response.Error(c, status, remote.Message)
		return
	}
	response.Error(c, http.StatusServiceUnavailable, "Host updater is unavailable")
}
