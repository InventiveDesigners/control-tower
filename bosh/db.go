package bosh

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"

	"strings"

	"golang.org/x/net/proxy"
)

type Opener interface {
	Open(name string) (*sql.DB, error)
	Close() error
}

type proxyOpener struct {
	baseURI string
	d       driver.Driver
	l       net.Listener
	start   func()
}

func (p *proxyOpener) Open(dbName string) (*sql.DB, error) {
	p.start()
	u, err := url.Parse(p.baseURI)
	if err != nil {
		return nil, err
	}
	u.Path = dbName
	u.Host = p.l.Addr().String()
	newURI := u.String()
	connector := connectorFunc(func(_ context.Context) (driver.Conn, error) {
		return p.d.Open(newURI)
	})
	return sql.OpenDB(connector), nil
}

func (p *proxyOpener) Close() error {
	p.start()
	return p.l.Close()
}

func newProxyOpener(p proxy.Dialer, d driver.Driver, uri string) (Opener, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var startOnce sync.Once
	f := func() {
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return //TODO: handle
				}
				go proxyConn(conn, p, u.Host)
			}
		}()
	}
	return &proxyOpener{
		baseURI: uri,
		d:       d,
		l:       l,
		start: func() {
			startOnce.Do(f)
		},
	}, nil
}

type connectorFunc func(context.Context) (driver.Conn, error)

func (f connectorFunc) Connect(ctx context.Context) (driver.Conn, error) {
	return f(ctx)
}

func (f connectorFunc) Driver() driver.Driver {
	panic("not supported")
}

func proxyConn(conn net.Conn, dialer proxy.Dialer, addr string) {
	defer conn.Close()
	target, err := dialer.Dial("tcp", addr)
	if err != nil {
		return //TODO: expose this error somehow
	}
	defer target.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(conn, target)
		wg.Done()
	}()
	go func() {
		io.Copy(target, conn)
		wg.Done()
	}()
	wg.Wait()
}

func (client *Client) createDefaultDatabases() error {
	db, err := client.db.Open(client.config.RDSDefaultDatabaseName)
	if err != nil {
		return err
	}
	defer db.Close()
	dbNames := []string{client.config.ConcourseDBName, "uaa", "credhub"}
	for _, dbName := range dbNames {
		_, err := db.Exec("CREATE DATABASE " + dbName)
		if err != nil && !strings.Contains(err.Error(),
			fmt.Sprintf(`pq: database "%s" already exists`, dbName)) {
			return err
		}
	}
	return nil
}

func (client *Client) stopCredhubAuditSpam() error {
	db, err := client.db.Open("credhub")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`
		TRUNCATE event_audit_record;
		TRUNCATE request_audit_record;
		CREATE OR REPLACE RULE dev_null_event_audit_record AS ON INSERT TO event_audit_record DO INSTEAD NOTHING;
		CREATE OR REPLACE RULE dev_null_request_audit_record AS ON INSERT TO request_audit_record DO INSTEAD NOTHING;`)
	return err
}
