package controlplane

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
)

type Server struct {
	manager  *MachineManager
	echo     *echo.Echo
	scriptMu sync.RWMutex
	scripts  map[string]string
}

func NewServer(manager *MachineManager) *Server {
	e := echo.New()

	srv := &Server{
		manager: manager,
		echo:    e,
		scripts: make(map[string]string),
	}

	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	e.POST("/machines", srv.handleCreateMachine)
	e.POST("/bootstrap", srv.handleRegisterScript)
	e.GET("/machines/:id", srv.handleStatus)
	e.GET("/machines/:id/console", srv.handleConsole)
	e.DELETE("/machines/:id", srv.handleDelete)

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

func (s *Server) handleCreateMachine(c echo.Context) error {
	var req MachineRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	if req.StartupScriptID != "" {
		script, ok := s.lookupScript(req.StartupScriptID)
		if !ok {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "unknown startup_script_id"})
		}
		req.StartupScript = script
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 90*time.Second)
	defer cancel()

	status, err := s.manager.CreateAndStart(ctx, req)
	if err != nil {
		if mErr, ok := err.(*MachineError); ok {
			return c.JSON(http.StatusInternalServerError, map[string]any{
				"error":  mErr.Error(),
				"events": mErr.Events,
			})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, status)
}

func (s *Server) handleStatus(c echo.Context) error {
	id := c.Param("id")
	ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
	defer cancel()

	status, err := s.manager.Status(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, status)
}

func (s *Server) handleDelete(c echo.Context) error {
	id := c.Param("id")
	ctx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
	defer cancel()

	if err := s.manager.Delete(ctx, id); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleConsole(c echo.Context) error {
	id := c.Param("id")
	request := c.Request()
	response := c.Response()

	handler := websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()

		ctx, cancel := context.WithCancel(request.Context())
		defer cancel()

		conn.PayloadType = websocket.TextFrame

		go func() {
			for {
				var discard []byte
				if err := websocket.Message.Receive(conn, &discard); err != nil {
					cancel()
					return
				}
			}
		}()

		if err := s.manager.StreamConsole(ctx, id, &websocketWriter{Conn: conn}); err != nil && !errors.Is(err, context.Canceled) {
			_ = websocket.Message.Send(conn, "error: "+err.Error())
		}
	})

	handler.ServeHTTP(response, request)
	return nil
}

type websocketWriter struct {
	sync.Mutex
	Conn *websocket.Conn
}

func (w *websocketWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	w.Lock()
	defer w.Unlock()
	return w.Conn.Write(p)
}

type startupScriptRequest struct {
	ID     string `json:"id"`
	Script string `json:"script"`
}

func (s *Server) handleRegisterScript(c echo.Context) error {
	var req startupScriptRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
	}
	if strings.TrimSpace(req.Script) == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "script is required"})
	}

	s.scriptMu.Lock()
	s.scripts[req.ID] = req.Script
	s.scriptMu.Unlock()

	return c.JSON(http.StatusOK, map[string]any{
		"id":     req.ID,
		"length": len(req.Script),
	})
}

func (s *Server) lookupScript(id string) (string, bool) {
	s.scriptMu.RLock()
	defer s.scriptMu.RUnlock()
	script, ok := s.scripts[id]
	return script, ok
}
