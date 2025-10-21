package controlplane

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

type deleteRequest struct {
	ID string `json:"id"`
}

func (s *Server) handleDeleteMergenVM(c echo.Context) error {
	var req deleteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if req.ID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()

	if err := s.manager.Delete(ctx, req.ID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "deleted"})
}
