package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ventrolab/ai-sre/pkg/enroll"
)

// enrollSecret runs as an init container (chart value enrollment.*). The agent keeps its
// credentials in a Kubernetes Secret, because a join token works only once:
//   - the Secret exists: copy its keys into the shared directory;
//   - otherwise enroll with the join token and save the result as that Secret.
//
// Keys: token auth "token" (and "ca.crt" with a CA pin); mtls "ca.crt", "tls.crt", "tls.key".
// Settings come from the environment: AUTH, HUB_URL, HUB_SERVER_NAME, JOIN_TOKEN, CA_SHA256,
// TLS_SECRET, POD_NAMESPACE, TLS_DIR.
func enrollSecret(ctx context.Context) error {
	ns, name, dir := os.Getenv("POD_NAMESPACE"), os.Getenv("TLS_SECRET"), os.Getenv("TLS_DIR")
	if ns == "" || name == "" || dir == "" {
		return errors.New("POD_NAMESPACE, TLS_SECRET and TLS_DIR are required")
	}
	auth := os.Getenv("AUTH")
	if auth == "" {
		auth = enroll.AuthToken
	}
	pin := os.Getenv("CA_SHA256")
	rc, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	files := enroll.Files{CA: filepath.Join(dir, "ca.crt"), Cert: filepath.Join(dir, "tls.crt"),
		Key: filepath.Join(dir, "tls.key"), Token: filepath.Join(dir, "token"), ServerName: os.Getenv("HUB_SERVER_NAME")}
	keys := map[string]string{files.Token: "token"}
	if auth == enroll.AuthMTLS {
		keys = map[string]string{files.CA: "ca.crt", files.Cert: "tls.crt", files.Key: "tls.key"}
	} else if pin != "" {
		keys[files.CA] = "ca.crt"
	}

	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		for path, key := range keys {
			if len(sec.Data[key]) == 0 {
				return fmt.Errorf("secret %s/%s has no %s (auth %s); delete it and enroll again with a new join token",
					ns, name, key, auth)
			}
			if err := os.WriteFile(path, sec.Data[key], 0o600); err != nil {
				return err
			}
		}
		fmt.Printf("credentials loaded from secret %s/%s\n", ns, name)
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("read secret %s/%s: %w", ns, name, err)
	}

	token := os.Getenv("JOIN_TOKEN")
	if token == "" || (auth == enroll.AuthMTLS && pin == "") {
		return fmt.Errorf("secret %s/%s does not exist and JOIN_TOKEN (or CA_SHA256 for mtls) is empty", ns, name)
	}
	ectx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	agent, err := enroll.Client(ectx, enroll.Options{HubURL: os.Getenv("HUB_URL"), JoinToken: token, CASHA256: pin,
		Auth: auth, Files: files})
	if err != nil {
		return err
	}
	data := map[string][]byte{}
	for path, key := range keys {
		if data[key], err = os.ReadFile(path); err != nil {
			return err
		}
	}
	_, err = cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns,
			Labels: map[string]string{"app.kubernetes.io/name": "k8s-agent", "ai-sre/agent": agent}},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}, metav1.CreateOptions{})
	if err != nil {
		// The credential exists only in this pod now; the next start needs a new join token.
		return fmt.Errorf("enrolled as %q but could not save secret %s/%s: %w", agent, ns, name, err)
	}
	fmt.Printf("enrolled as %q (auth %s), credentials saved to secret %s/%s\n", agent, auth, ns, name)
	return nil
}
