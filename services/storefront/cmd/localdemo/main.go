// localdemo is a separate, loopback-only executable. Never deploy this command
// as a production API or supply production database credentials to it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gptibo/storefront/internal/localdemo"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "Local demo failed:", e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: localdemo /absolute/path/to/private-config.json")
	}
	st, e := os.Stat(os.Args[1])
	if e != nil {
		return errors.New("demo configuration missing")
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("demo configuration must be owner-only (chmod 600)")
	}
	b, e := os.ReadFile(os.Args[1])
	if e != nil {
		return errors.New("demo configuration unreadable")
	}
	var c localdemo.Config
	if json.Unmarshal(b, &c) != nil {
		return errors.New("invalid demo configuration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, e := localdemo.New(ctx, c, slog.Default())
	if e != nil {
		// SQL/driver errors can contain data. Keep diagnostics in an owner-only file.
		_ = os.WriteFile(os.Args[1]+".error", []byte(e.Error()), 0600)
		return errors.New("startup failed; inspect the private config .error file")
	}
	defer s.Store.DB.Close()
	server := &http.Server{Addr: c.Listen, Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	fmt.Println("GPTibo local demo listening at", c.Origin, "(all external services simulated)")
	select {
	case e = <-done:
		if !errors.Is(e, http.ErrServerClosed) {
			return errors.New("listener unavailable")
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
	return nil
}
