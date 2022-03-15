package coraza

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/corazawaf/coraza-waf/v2"
	"github.com/corazawaf/coraza-waf/v2/seclang"
	"github.com/corazawaf/coraza-waf/v2/types"
	_ "github.com/fog94/coraza-wace/corazawace"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Middleware{})
	httpcaddyfile.RegisterHandlerDirective("coraza_waf", parseCaddyfile)
}

type Middleware struct {
	Include    []string `json:"include"`
	Directives string   `json:"directives"`

	logger *zap.Logger
	waf    *coraza.Waf
}

// CaddyModule returns the Caddy module information.
func (Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.waf",
		New: func() caddy.Module { return new(Middleware) },
	}
}

// Provision implements caddy.Provisioner.
func (m *Middleware) Provision(ctx caddy.Context) error {
	var err error
	m.logger = ctx.Logger(m)
	m.waf = coraza.NewWaf()
	m.waf.SetErrorLogCb(logger(m.logger))
	pp, _ := seclang.NewParser(m.waf)
	if m.Directives != "" {
		if err = pp.FromString(m.Directives); err != nil {
			return err
		}
	}
	m.logger.Debug("Preparing to include files", zap.Int("count", len(m.Include)), zap.Strings("files", m.Include))
	if len(m.Include) > 0 {
		for _, file := range m.Include {
			if strings.Contains(file, "*") {
				m.logger.Debug("Preparing to expand glob", zap.String("pattern", file))
				// we get files as expandables globs (with wildcard patterns)
				fs, err := filepath.Glob(file)
				if err != nil {
					return err
				}
				m.logger.Debug("Glob expanded", zap.String("pattern", file), zap.Strings("files", fs))
				for _, f := range fs {
					if err := pp.FromFile(f); err != nil {
						return err
					}
				}
			} else {
				m.logger.Debug("File was not a pattern, compiling it", zap.String("file", file))
				if err := pp.FromFile(file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Validate implements caddy.Validator.
func (m *Middleware) Validate() error {
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	var err error
	tx := m.waf.NewTransaction()
	defer tx.ProcessLogging()
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	repl.Set("http.transaction_id", tx.ID)

	it, err := tx.ProcessRequest(r)
	if err != nil {
		return err
	}
	if it != nil {
		return interrupt(nil, tx)
	}

	// TODO this is a temporal fix while I fix it in coraza
	re, err := tx.RequestBodyBuffer.Reader()
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(re)
	rec := NewStreamRecorder(w, tx)
	err = next.ServeHTTP(rec, r)
	if err != nil {
		return err
	}
	// If the response was interrupted during phase 3 or 4 we can stop the response
	if tx.Interruption != nil {
		return interrupt(nil, tx)
	}
	if !rec.Buffered() {
		//Nothing to do, response was already sent to the client
		return nil
	}

	if status := rec.Status(); status > 0 {
		w.WriteHeader(status)
	}
	// We will send the response provided by Coraza
	reader, err := rec.Reader()
	if err != nil {
		return err
	}
	_, err = io.Copy(w, reader)
	return err
}

// Unmarshal Caddyfile implements caddyfile.Unmarshaler.
func (m *Middleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() {
		return d.Err("expected token following filter")
	}
	m.Include = []string{}
	for d.NextBlock(0) {
		key := d.Val()
		var value string
		d.Args(&value)
		if d.NextArg() {
			return d.ArgErr()
		}
		switch key {
		case "include":
			m.Include = append(m.Include, value)
		case "directives":
			m.Directives = value
		default:
			return d.Err(fmt.Sprintf("invalid key for filter directive: %s", key))
		}
	}
	return nil
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m Middleware
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

func logger(logger *zap.Logger) coraza.ErrorLogCallback {
	return func(mr coraza.MatchedRule) {
		data := mr.ErrorLog(403)
		switch mr.Rule.Severity {
		case types.RuleSeverityEmergency:
			logger.Error(data)
		case types.RuleSeverityAlert:
			logger.Error(data)
		case types.RuleSeverityCritical:
			logger.Error(data)
		case types.RuleSeverityError:
			logger.Error(data)
		case types.RuleSeverityWarning:
			logger.Warn(data)
		case types.RuleSeverityNotice:
			logger.Info(data)
		case types.RuleSeverityInfo:
			logger.Info(data)
		case types.RuleSeverityDebug:
			logger.Debug(data)
		}
	}
}

func interrupt(err error, tx *coraza.Transaction) error {
	if tx.Interruption == nil {
		return caddyhttp.HandlerError{
			StatusCode: 500,
			ID:         tx.ID,
			Err:        err,
		}
	}
	status := tx.Interruption.Status
	if status <= 0 {
		status = 403
	}
	return caddyhttp.HandlerError{
		StatusCode: status,
		ID:         tx.ID,
		Err:        err,
	}
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Middleware)(nil)
	_ caddy.Validator             = (*Middleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
	_ caddyfile.Unmarshaler       = (*Middleware)(nil)
)
