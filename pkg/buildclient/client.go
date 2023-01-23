package buildclient

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	apiv1 "github.com/acorn-io/acorn/pkg/apis/api.acorn.io/v1"
	v1 "github.com/acorn-io/acorn/pkg/apis/internal.acorn.io/v1"
	"github.com/acorn-io/acorn/pkg/streams"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func wsURL(url string) string {
	if strings.HasPrefix(url, "http") {
		return strings.Replace(url, "http", "ws", 1)
	}
	return url
}

type CredentialLookup func(ctx context.Context, serverAddress string) (*apiv1.RegistryAuth, bool, error)

type WebSocketDialer func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error)

func Stream(ctx context.Context, cwd string, streams *streams.Output, dialer WebSocketDialer,
	creds CredentialLookup, build *apiv1.AcornImageBuild) (*v1.AppImage, error) {
	conn, _, err := dialer(ctx, wsURL(build.Status.BuildURL), map[string][]string{
		"X-Acorn-Build-Token": {build.Status.Token},
	})
	if err != nil {
		return nil, err
	}

	var (
		messages = NewWebsocketMessages(conn)
		syncers  = map[string]*fileSyncClient{}
	)
	defer func() {
		for _, s := range syncers {
			s.Close()
		}
	}()
	defer messages.Close()

	msgs, cancel := messages.Recv()
	defer cancel()

	progress := newClientProgress(ctx, streams)
	defer progress.Close()

	// Handle messages synchronous since new subscribers are started,
	// and we don't want to miss a message.
	messages.OnMessage(func(msg *Message) error {
		if msg.FileSessionID == "" {
			return nil
		}
		if _, ok := syncers[msg.FileSessionID]; ok {
			return nil
		}
		s, err := newFileSyncClient(ctx, cwd, msg.FileSessionID, messages, msg.SyncOptions)
		if err != nil {
			return err
		}
		syncers[msg.FileSessionID] = s
		return nil
	})

	messages.Start(ctx)

	for msg := range msgs {
		if msg.Status != nil {
			progress.Display(msg)
		} else if msg.AppImage != nil {
			return msg.AppImage, nil
		} else if msg.RegistryServerAddress != "" {
			err := messages.Send(lookupCred(ctx, creds, msg.RegistryServerAddress))
			if err != nil {
				return nil, err
			}
		} else if msg.Error != "" {
			return nil, errors.New(msg.Error)
		}
	}

	return nil, fmt.Errorf("build failed")
}

func lookupCred(ctx context.Context, creds CredentialLookup, serverAddress string) (result *Message) {
	result = &Message{
		RegistryServerAddress: serverAddress,
	}

	if creds == nil {
		return
	}

	cred, found, err := creds(ctx, serverAddress)
	if err != nil {
		logrus.Errorf("failed to lookup credential for server address %s: %v", serverAddress, err)
		return
	} else if !found {
		return
	}

	result.RegistryAuth = &apiv1.RegistryAuth{
		Username: cred.Username,
		Password: cred.Password,
	}
	return
}

func PingBuilder(ctx context.Context, baseURL string) bool {
	for i := 0; i < 5; i++ {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/ping", nil)
		if err != nil {
			logrus.Debugf("failed to build request for builder ping to %s: %v", baseURL+"/ping", err)
			return false
		}

		subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := http.DefaultClient.Do(req.WithContext(subCtx))
		cancel()
		if err != nil {
			logrus.Debugf("builder ping failed: %v", err)
		} else {
			_ = resp.Body.Close()
			logrus.Debugf("builder status code: %d", resp.StatusCode)
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(time.Second)
	}
	return false
}
