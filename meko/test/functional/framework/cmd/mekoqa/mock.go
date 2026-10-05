package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	cognitoserver "github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock/server"
	llmserver "github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock/server"
)

func newMockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mock",
		Short: "Run one of the stack's mock services",
		Long: "The utility apps of a test stack, in the framework's own binary. The stack runs them in pods\n" +
			"(the binary is copied in), and they run locally the same way for debugging.",
	}
	cmd.AddCommand(newMockLLMCmd(), newMockCognitoCmd())
	return cmd
}

func logStderr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func newMockLLMCmd() *cobra.Command {
	port := 8080
	if v, err := strconv.Atoi(os.Getenv("LLMMOCK_PORT")); err == nil {
		port = v
	}
	host := "0.0.0.0"
	cmd := &cobra.Command{
		Use:   "llm",
		Short: "OpenAI and AWS Bedrock Runtime mock",
		Long: "Serves /v1/embeddings, /v1/chat/completions (also streaming), /v1/responses, /v1/models and Bedrock\n" +
			"Converse, ConverseStream and InvokeModel. /_mock/* is the control API: scripted replies, embedding\n" +
			"fixtures, fault injection and the recorded calls.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return llmserver.New(logStderr).Serve(c.Context(), net.JoinHostPort(host, strconv.Itoa(port)))
		},
	}
	cmd.Flags().StringVar(&host, "host", host, "address to listen on")
	cmd.Flags().IntVar(&port, "port", port, "port to listen on (default from $LLMMOCK_PORT)")
	return cmd
}

func newMockCognitoCmd() *cobra.Command {
	var (
		region, poolID, clientID, clientSecret string
		code, kid                              string
		tlsPort, ctlPort                       int
		certFile, keyFile, tokenKey, state     string
		ttl                                    time.Duration
		host                                   string
	)
	cmd := &cobra.Command{
		Use:   "cognito",
		Short: "Cognito user pool mock",
		Long: "Two listeners. The public one answers the Cognito JSON API (X-Amz-Target) and the pool's JWKS over\n" +
			"TLS, as cognito-idp.<region>.amazonaws.com. The control one carries /control/users, /control/tokens\n" +
			"and /healthz over plain HTTP. Without --tls-cert the public listener is plain HTTP, and without\n" +
			"--token-key an ephemeral signing key is generated, which is enough for running it by hand.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			key, err := loadTokenKey(tokenKey)
			if err != nil {
				return err
			}
			if clientSecret == "" {
				clientSecret = os.Getenv("COGNITO_MOCK_CLIENT_SECRET")
			}
			pool, err := cognitoserver.New(cognitoserver.Config{
				Region: region, PoolID: poolID, ClientID: clientID, ClientSecret: clientSecret,
				TokenTTL: ttl, ConfirmationCode: code, KID: kid, Key: key, StatePath: state, Logf: logStderr,
			})
			if err != nil {
				return err
			}
			return pool.Serve(c.Context(), cognitoserver.Listeners{
				PublicAddr:  net.JoinHostPort(host, strconv.Itoa(tlsPort)),
				ControlAddr: net.JoinHostPort(host, strconv.Itoa(ctlPort)),
				CertFile:    certFile, KeyFile: keyFile,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&host, "host", "0.0.0.0", "address to listen on")
	f.StringVar(&region, "region", "us-east-1", "region in the issuer and the served hostname")
	f.StringVar(&poolID, "pool-id", "us-east-1_MekoQa001", "user pool id")
	f.StringVar(&clientID, "client-id", "mekoqa0client0id0000000000", "app client id")
	f.StringVar(&clientSecret, "client-secret", "", "app client secret, SECRET_HASH is enforced when set (default from $COGNITO_MOCK_CLIENT_SECRET)")
	f.DurationVar(&ttl, "token-ttl", 24*time.Hour, "lifetime of the tokens it signs")
	f.StringVar(&code, "code", "123456", "the confirmation code it accepts everywhere Cognito would email one")
	f.StringVar(&kid, "kid", "mekoqa-1", "key id in the token header and the JWKS")
	f.IntVar(&tlsPort, "tls-port", 8443, "port of the public listener")
	f.IntVar(&ctlPort, "ctl-port", 8080, "port of the control listener")
	f.StringVar(&certFile, "tls-cert", "", "serving certificate for the public listener, empty serves plain HTTP")
	f.StringVar(&keyFile, "tls-key", "", "private key of --tls-cert")
	f.StringVar(&tokenKey, "token-key", "", "RSA private key (PEM) the tokens are signed with, empty generates one")
	f.StringVar(&state, "state", "", "file that keeps users across restarts, empty keeps them in memory")
	return cmd
}

func loadTokenKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return rsa.GenerateKey(rand.Reader, 2048)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s is neither a PKCS#1 nor a PKCS#8 key", path)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New(path + " is not an RSA key")
	}
	return rk, nil
}
