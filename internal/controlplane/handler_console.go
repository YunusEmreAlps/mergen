package controlplane

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

type hijackedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *hijackedConn) Write(p []byte) (int, error) {
	return c.Conn.Write(p)
}

func (s *Server) handleConsoleMergenVM(c echo.Context) error {
	id := c.Param("id")
	request := c.Request()

	hijacker, ok := c.Response().Writer.(http.Hijacker)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "connection hijacking not supported")
	}

	rawConn, rw, err := hijacker.Hijack()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()

	fcConn, err := s.manager.DialConsole(ctx, id)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeConsoleError(rw, rawConn, status, err.Error())
		return nil
	}

	if err := writeConsoleUpgrade(rw); err != nil {
		fcConn.Close()
		rawConn.Close()
		return nil
	}

	clientConn := &hijackedConn{Conn: rawConn, reader: rw.Reader}

	errCh := make(chan error, 2)

	go func() {
		_, copyErr := io.Copy(fcConn, clientConn)
		errCh <- copyErr
	}()

	go func() {
		_, copyErr := io.Copy(clientConn, fcConn)
		errCh <- copyErr
	}()

	select {
	case <-ctx.Done():
	case <-errCh:
	}

	fcConn.Close()
	rawConn.Close()

	return nil
}

func writeConsoleUpgrade(rw *bufio.ReadWriter) error {
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return err
	}
	if _, err := rw.WriteString("Upgrade: mergen-console\r\n"); err != nil {
		return err
	}
	if _, err := rw.WriteString("Connection: Upgrade\r\n\r\n"); err != nil {
		return err
	}
	return rw.Flush()
}

func writeConsoleError(rw *bufio.ReadWriter, conn net.Conn, status int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(rw, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	fmt.Fprintf(rw, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(rw, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(rw, "Connection: close\r\n\r\n")
	rw.WriteString(body)
	_ = rw.Flush()
	_ = conn.Close()
}
