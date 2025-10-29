package controlplane

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

type Server struct {
	manager *MachineManager
	echo    *echo.Echo
}

func NewServer(manager *MachineManager) *Server {
	e := echo.New()

	srv := &Server{
		manager: manager,
		echo:    e,
	}

	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	e.POST("/create/:name", srv.handleCreateVM)
	e.POST("/stop/:name", srv.handleStopVM)
	e.DELETE("/delete/:name", srv.handleDeleteVM)
	e.GET("/console/:id", srv.handleConsoleVM)

	return srv
}

func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := s.echo

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
