package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"runtime/trace"
	"sort"

	"github.com/foxcpp/maddy/internal/config"
	"github.com/foxcpp/maddy/internal/exterrors"
	"github.com/foxcpp/maddy/internal/smtpconn"
)

type mxConn struct {
	*smtpconn.C

	// Domain this MX belongs to.
	domain   string
	dnssecOk bool
}

func isVerifyError(err error) bool {
	_, ok := err.(x509.UnknownAuthorityError)
	if ok {
		return true
	}
	_, ok = err.(x509.HostnameError)
	if ok {
		return true
	}
	_, ok = err.(x509.ConstraintViolationError)
	if ok {
		return true
	}
	_, ok = err.(x509.CertificateInvalidError)
	return ok
}

// connect attempts to connect to the MX, first trying STARTTLS with X.509
// verification but falling back to unauthenticated TLS or plaintext as
// necessary.
//
// Return values:
// - tlsLevel    TLS security level that was estabilished.
// - tlsErr      Error that prevented TLS from working if tlsLevel != TLSAuthenticated
func (rd *remoteDelivery) connect(ctx context.Context, conn mxConn, host string, tlsCfg *tls.Config) (tlsLevel TLSLevel, tlsErr error, err error) {
	tlsLevel = TLSAuthenticated
	if rd.rt.tlsConfig != nil {
		tlsCfg = rd.rt.tlsConfig.Clone()
		tlsCfg.ServerName = host
	}

	rd.Log.DebugMsg("trying", "remote_server", host, "domain", conn.domain)

retry:
	// smtpconn.C default TLS behavior is not useful for us, we want to handle
	// TLS errors separately hence starttls=false.
	_, err = conn.Connect(ctx, config.Endpoint{
		Host: host,
		Port: smtpPort,
	}, false, nil)
	if err != nil {
		return TLSNone, nil, err
	}

	starttlsOk, _ := conn.Client().Extension("STARTTLS")
	if starttlsOk && tlsCfg != nil {
		if err := conn.Client().StartTLS(tlsCfg); err != nil {
			tlsErr = err

			// Attempt TLS without authentication. It is still better than
			// plaintext and we might be able to actually authenticate the
			// server using DANE-EE/DANE-TA later.
			//
			// Check tlsLevel is to avoid looping forever if the same verify
			// error happens with InsecureSkipVerify too (e.g. certificate is
			// *too* broken).
			if isVerifyError(err) && tlsLevel == TLSAuthenticated {
				rd.Log.Error("TLS verify error, trying without authentication", err, "remote_server", host, "domain", conn.domain)
				tlsCfg.InsecureSkipVerify = true
				tlsLevel = TLSEncrypted

				// TODO: Check go-smtp code to make TLS verification errors
				// non-sticky so we can properly send QUIT in this case.
				conn.DirectClose()

				goto retry
			}

			rd.Log.Error("TLS error, trying plaintext", err, "remote_server", host, "domain", conn.domain)
			tlsCfg = nil
			tlsLevel = TLSNone
			conn.DirectClose()

			goto retry
		}
	} else {
		tlsLevel = TLSNone
	}

	return tlsLevel, tlsErr, nil
}

func (rd *remoteDelivery) attemptMX(ctx context.Context, conn mxConn, record *net.MX) error {
	mxLevel := MXNone

	connCtx, cancel := context.WithCancel(ctx)
	// Cancel async policy lookups if rd.connect fails.
	defer cancel()

	for _, p := range rd.policies {
		policyLevel, err := p.CheckMX(connCtx, mxLevel, conn.domain, record.Host, conn.dnssecOk)
		if err != nil {
			return err
		}
		if policyLevel > mxLevel {
			mxLevel = policyLevel
		}

		p.PrepareConn(ctx, record.Host)
	}

	tlsLevel, tlsErr, err := rd.connect(connCtx, conn, record.Host, rd.rt.tlsConfig)
	if err != nil {
		return err
	}

	// Make decision based on the policy and connection state.
	//
	// Note: All policy errors are marked as temporary to give the local admin
	// chance to troubleshoot them without losing messages.

	tlsState, _ := conn.Client().TLSConnectionState()
	for _, p := range rd.policies {
		policyLevel, err := p.CheckConn(connCtx, mxLevel, tlsLevel, conn.domain, record.Host, tlsState)
		if err != nil {
			conn.Close()
			return exterrors.WithFields(err, map[string]interface{}{"tls_err": tlsErr})
		}
		if policyLevel > tlsLevel {
			tlsLevel = policyLevel
		}
	}

	return nil
}

func (rd *remoteDelivery) connectionForDomain(ctx context.Context, domain string) (*smtpconn.C, error) {
	if c, ok := rd.connections[domain]; ok {
		return c.C, nil
	}

	conn := mxConn{
		C:      smtpconn.New(),
		domain: domain,
	}

	conn.Dialer = rd.rt.dialer
	conn.Log = rd.Log
	conn.Hostname = rd.rt.hostname
	conn.AddrInSMTPMsg = true

	for _, p := range rd.policies {
		p.PrepareDomain(ctx, domain)
	}

	region := trace.StartRegion(ctx, "remote/LookupMX")
	dnssecOk, records, err := rd.lookupMX(ctx, domain)
	region.End()
	if err != nil {
		return nil, err
	}
	conn.dnssecOk = dnssecOk

	region = trace.StartRegion(ctx, "remote/limits.TakeDest")
	if err := rd.rt.limits.TakeDest(ctx, domain); err != nil {
		region.End()
		return nil, err
	}
	region.End()

	var lastErr error
	region = trace.StartRegion(ctx, "remote/Connect+TLS")
	for _, record := range records {
		if record.Host == "." {
			return nil, &exterrors.SMTPError{
				Code:         556,
				EnhancedCode: exterrors.EnhancedCode{5, 1, 10},
				Message:      "Domain does not accept email (null MX)",
			}
		}

		if err := rd.attemptMX(ctx, conn, record); err != nil {
			if len(records) != 0 {
				rd.Log.Error("cannot use MX", err, "remote_server", record.Host, "domain", domain)
			}
			lastErr = err
			continue
		}
		break
	}
	region.End()

	// Stil not connected? Bail out.
	if conn.Client() == nil {
		return nil, &exterrors.SMTPError{
			Code:         exterrors.SMTPCode(lastErr, 451, 550),
			EnhancedCode: exterrors.SMTPEnchCode(lastErr, exterrors.EnhancedCode{0, 4, 0}),
			Message:      "No usable MXs, last err: " + lastErr.Error(),
			TargetName:   "remote",
			Err:          lastErr,
			Misc: map[string]interface{}{
				"domain": domain,
			},
		}
	}

	if err := conn.Mail(ctx, rd.mailFrom, rd.msgMeta.SMTPOpts); err != nil {
		conn.Close()
		return nil, err
	}

	rd.connections[domain] = conn
	return conn.C, nil
}

func (rd *remoteDelivery) lookupMX(ctx context.Context, domain string) (dnssecOk bool, records []*net.MX, err error) {
	if rd.rt.extResolver != nil {
		dnssecOk, records, err = rd.rt.extResolver.AuthLookupMX(context.Background(), domain)
	} else {
		records, err = rd.rt.resolver.LookupMX(ctx, domain)
	}
	if err != nil {
		reason, misc := exterrors.UnwrapDNSErr(err)
		return false, nil, &exterrors.SMTPError{
			Code:         exterrors.SMTPCode(err, 451, 554),
			EnhancedCode: exterrors.SMTPEnchCode(err, exterrors.EnhancedCode{0, 4, 4}),
			Message:      "MX lookup error",
			TargetName:   "remote",
			Reason:       reason,
			Err:          err,
			Misc:         misc,
		}
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Pref < records[j].Pref
	})

	// Fallback to A/AAA RR when no MX records are present as
	// required by RFC 5321 Section 5.1.
	if len(records) == 0 {
		records = append(records, &net.MX{
			Host: domain,
			Pref: 0,
		})
	}

	return dnssecOk, records, err
}
