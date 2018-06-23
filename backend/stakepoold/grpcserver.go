// Copyright (c) 2013-2015 The btcsuite developers
// Copyright (c) 2015-2017 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"crypto/elliptic"
	"crypto/tls"
	"errors"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	xcontext "golang.org/x/net/context"

	"github.com/coolsnady/hxd/certgen"
	"github.com/coolsnady/hxstakepool/backend/stakepoold/rpc/rpcserver"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// generateRPCKeyPair generates a new RPC TLS keypair and writes the cert and
// possibly also the key in PEM format to the paths specified by the config.  If
// successful, the new keypair is returned.
func generateRPCKeyPair(writeKey bool) (tls.Certificate, error) {
	log.Info("Generating TLS certificates...")

	// Create directories for cert and key files if they do not yet exist.
	certDir, _ := filepath.Split(cfg.RPCCert)
	keyDir, _ := filepath.Split(cfg.RPCKey)
	err := os.MkdirAll(certDir, 0700)
	if err != nil {
		return tls.Certificate{}, err
	}
	err = os.MkdirAll(keyDir, 0700)
	if err != nil {
		return tls.Certificate{}, err
	}

	// Generate cert pair.
	org := "stakepoold autogenerated cert"
	validUntil := time.Now().Add(time.Hour * 24 * 365 * 10)
	cert, key, err := certgen.NewTLSCertPair(elliptic.P521(), org,
		validUntil, nil)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	// Write cert and (potentially) the key files.
	err = ioutil.WriteFile(cfg.RPCCert, cert, 0600)
	if err != nil {
		return tls.Certificate{}, err
	}
	if writeKey {
		err = ioutil.WriteFile(cfg.RPCKey, key, 0600)
		if err != nil {
			rmErr := os.Remove(cfg.RPCCert)
			if rmErr != nil {
				log.Warnf("Cannot remove written certificates: %v",
					rmErr)
			}
			return tls.Certificate{}, err
		}
	}

	log.Info("Done generating TLS certificates")
	return keyPair, nil
}

func interceptUnary(ctx xcontext.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	startTime := time.Now()

	// parse out method from '/package.service/method'
	methodSplit := strings.SplitAfterN(info.FullMethod, "/", 3)
	method := methodSplit[2]
	peer, peerOk := peer.FromContext(ctx)

	// limit the time we take
	ctx, cancel := context.WithTimeout(ctx, rpcserver.GRPCCommandTimeout)
	// it is good practice to use the cancellation function even with a timeout
	defer cancel()

	resp, err = handler(ctx, req)
	if err != nil && peerOk {
		grpcLog.Errorf("%s invoked by %s failed: %v",
			method, peer.Addr.String(), err)
	}

	defer func() {
		if peerOk {
			grpcLog.Infof("%s invoked by %s processed in %v", method,
				peer.Addr.String(), time.Since(startTime))
		} else {
			grpcLog.Infof("%s processed in %v", method,
				time.Since(startTime))
		}
	}()
	return resp, err
}

type listenFunc func(net string, laddr string) (net.Listener, error)

// makeListeners splits the normalized listen addresses into IPv4 and IPv6
// addresses and creates new net.Listeners for each with the passed listen func.
// Invalid addresses are logged and skipped.
func makeListeners(normalizedListenAddrs []string, listen listenFunc) []net.Listener {
	ipv4Addrs := make([]string, 0, len(normalizedListenAddrs)*2)
	ipv6Addrs := make([]string, 0, len(normalizedListenAddrs)*2)
	for _, addr := range normalizedListenAddrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			// Shouldn't happen due to already being normalized.
			log.Errorf("`%s` is not a normalized "+
				"listener address", addr)
			continue
		}

		// Empty host or host of * on plan9 is both IPv4 and IPv6.
		if host == "" || (host == "*" && runtime.GOOS == "plan9") {
			ipv4Addrs = append(ipv4Addrs, addr)
			ipv6Addrs = append(ipv6Addrs, addr)
			continue
		}

		// Remove the IPv6 zone from the host, if present.  The zone
		// prevents ParseIP from correctly parsing the IP address.
		// ResolveIPAddr is intentionally not used here due to the
		// possibility of leaking a DNS query over Tor if the host is a
		// hostname and not an IP address.
		zoneIndex := strings.Index(host, "%")
		if zoneIndex != -1 {
			host = host[:zoneIndex]
		}

		ip := net.ParseIP(host)
		switch {
		case ip == nil:
			log.Warnf("`%s` is not a valid IP address", host)
		case ip.To4() == nil:
			ipv6Addrs = append(ipv6Addrs, addr)
		default:
			ipv4Addrs = append(ipv4Addrs, addr)
		}
	}
	listeners := make([]net.Listener, 0, len(ipv6Addrs)+len(ipv4Addrs))
	for _, addr := range ipv4Addrs {
		listener, err := listen("tcp4", addr)
		if err != nil {
			log.Warnf("Can't listen on %s: %v", addr, err)
			continue
		}
		listeners = append(listeners, listener)
	}
	for _, addr := range ipv6Addrs {
		listener, err := listen("tcp6", addr)
		if err != nil {
			log.Warnf("Can't listen on %s: %v", addr, err)
			continue
		}
		listeners = append(listeners, listener)
	}
	return listeners
}

// openRPCKeyPair creates or loads the RPC TLS keypair specified by the
// application config.
func openRPCKeyPair() (tls.Certificate, error) {
	// Generate a new keypair when the key is missing.
	_, e := os.Stat(cfg.RPCKey)
	keyExists := !os.IsNotExist(e)
	if !keyExists {
		return generateRPCKeyPair(true)
	}

	return tls.LoadX509KeyPair(cfg.RPCCert, cfg.RPCKey)
}

func startGRPCServers(grpcCommandQueueChan chan *rpcserver.GRPCCommandQueue) (*grpc.Server, error) {
	var (
		server  *grpc.Server
		keyPair tls.Certificate
		err     error
	)

	keyPair, err = openRPCKeyPair()
	if err != nil {
		return nil, err
	}

	listeners := makeListeners(cfg.RPCListeners, net.Listen)
	if len(listeners) == 0 {
		err := errors.New("failed to create listeners for RPC server")
		return nil, err
	}
	creds := credentials.NewServerTLSFromCert(&keyPair)
	server = grpc.NewServer(grpc.Creds(creds), grpc.UnaryInterceptor(interceptUnary))
	rpcserver.StartVersionService(server)
	rpcserver.StartStakepooldService(grpcCommandQueueChan, server)
	for _, lis := range listeners {
		lis := lis
		go func() {
			log.Infof("gRPC server listening on %s",
				lis.Addr())
			err := server.Serve(lis)
			log.Tracef("Finished serving gRPC: %v",
				err)
		}()
	}

	// Error when the server can't be started
	if server == nil {
		return nil, errors.New("gRPC service cannot be started")
	}

	return server, nil
}