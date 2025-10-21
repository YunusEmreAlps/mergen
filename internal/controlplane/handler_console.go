package controlplane

import (
	"context"
	"errors"
	"io"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/websocket"
)

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
