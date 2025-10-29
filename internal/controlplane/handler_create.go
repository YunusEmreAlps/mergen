package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

type createRequest struct {
	CPUCount  int64 `json:"cpu_count"`
	MemSizeMb int64 `json:"mem_size_mb"`
}

func (s *Server) handleCreateVM(c echo.Context) error {
	name := c.Param("name")
	var payload createRequest
	if err := c.Bind(&payload); err != nil && !errors.Is(err, io.EOF) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	status, err := s.manager.Create(ctx, name, MachineSpec{
		CPUCount:  payload.CPUCount,
		MemSizeMb: payload.MemSizeMb,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		return c.JSON(status, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, status)
}
