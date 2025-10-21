package controlplane

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

func (s *Server) handleCreateMergenVM(c echo.Context) error {
	var req MachineRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	status, err := s.manager.CreateAndStart(ctx, req)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, status)
}
