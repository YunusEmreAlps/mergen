package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
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

	e.POST("/CreateMergenVM", srv.handleCreateMergenVM)
	e.POST("/DeleteMergenVM", srv.handleDeleteMergenVM)
	e.GET("/ConsoleMergenVM/:id", srv.handleConsoleMergenVM)

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

type deleteRequest struct {
	ID string `json:"id"`
}

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

func (s *Server) handleConsoleMergenVM(c echo.Context) error {
	id := c.Param("id")
	request := c.Request()
	response := c.Response()

	handler := websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()

		ctx, cancel := context.WithCancel(request.Context())
		defer cancel()

		fcConn, err := s.manager.DialConsole(ctx, id)
		if err != nil {
			_ = websocket.Message.Send(conn, "error: "+err.Error())
			return
		}
		defer fcConn.Close()

		conn.PayloadType = websocket.BinaryFrame

		errCh := make(chan error, 2)

		go func() {
			_, err := io.Copy(fcConn, conn)
			errCh <- err
		}()

		go func() {
			_, err := io.Copy(conn, fcConn)
			errCh <- err
		}()

		select {
		case <-ctx.Done():
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) && err != io.EOF {
				_ = websocket.Message.Send(conn, "error: "+err.Error())
			}
		}
	})

	handler.ServeHTTP(response, request)
	return nil
}
