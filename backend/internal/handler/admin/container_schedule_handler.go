package admin

import (
	"context"
	"net/http"
	"regexp"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type containerScheduleService interface {
	ContainerMode() bool
	ContainerSchedule(context.Context) (*kanoupdate.Session, error)
	ContainerCreateSchedule(context.Context, kanoupdate.StartRequest) (*kanoupdate.Session, error)
	ContainerCancelSchedule(context.Context, string) error
}

func (h *SystemHandler) scheduleUpdater(c *gin.Context) containerScheduleService {
	updater, ok := h.updateSvc.(containerScheduleService)
	if !ok || !updater.ContainerMode() {
		response.ErrorFrom(c, service.ErrForkUpdateManaged)
		return nil
	}
	return updater
}

func (h *SystemHandler) CreateUpdateSchedule(c *gin.Context) {
	updater := h.scheduleUpdater(c)
	if updater == nil {
		return
	}
	performContainerAction(c, updater.ContainerCreateSchedule)
}

func (h *SystemHandler) UpdateSchedule(c *gin.Context) {
	updater := h.scheduleUpdater(c)
	if updater == nil {
		return
	}
	session, err := updater.ContainerSchedule(c.Request.Context())
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

var scheduleIDPattern = regexp.MustCompile(`^up-[a-f0-9]{32}$`)

func (h *SystemHandler) CancelUpdateSchedule(c *gin.Context) {
	updater := h.scheduleUpdater(c)
	if updater == nil {
		return
	}
	id := c.Param("id")
	if !scheduleIDPattern.MatchString(id) {
		response.Error(c, http.StatusBadRequest, "Invalid reservation")
		return
	}
	if err := updater.ContainerCancelSchedule(c.Request.Context(), id); err != nil {
		containerUpdateError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"cancelled": true})
}
