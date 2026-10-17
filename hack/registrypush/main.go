// Command registrypush pushes one image to a registry through the bundled
// registry's front proxy and prints its digest. The install and check scripts
// call it through a kubectl port-forward on localhost. The credentials come from
// LAST_REGISTRY_USERNAME and LAST_REGISTRY_PASSWORD (both empty means anonymous)
// and are never printed. Stdout is only the digest and a newline.
//
//	go run ./hack/registrypush --to HOST/REPO:TAG (--tar FILE | --from REF [--platform OS/ARCH]) [--label KEY=VALUE]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/moogie-labs/last-operator/internal/images"
)

const usage = "Usage: registrypush --to HOST/REPO:TAG (--tar FILE | --from REF [--platform OS/ARCH])" +
	" [--label KEY=VALUE]"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("registrypush", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "destination HOST/REPO:TAG")
	tar := fs.String("tar", "", "docker save archive to push")
	from := fs.String("from", "", "image reference to pull and push")
	platform := fs.String("platform", "linux/amd64", "platform to pull with --from")
	label := fs.String("label", "", "config label KEY=VALUE")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	if *to == "" || (*tar == "") == (*from == "") {
		_, _ = fmt.Fprintln(stderr, usage)
		return 1
	}
	digest, err := push(*to, *tar, *from, *platform, *label, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, digest)
	return 0
}

func push(to, tarFile, from, platform, label string, getenv func(string) string) (string, error) {
	host, repo, tag, err := splitDestination(to)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var img v1.Image
	if tarFile != "" {
		img, err = tarball.ImageFromPath(tarFile, nil)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", tarFile, err)
		}
	} else {
		img, err = pull(ctx, from, platform)
		if err != nil {
			return "", err
		}
	}
	if label != "" {
		img, err = withLabel(img, label)
		if err != nil {
			return "", err
		}
	}

	var creds *images.Credentials
	if u, p := getenv("LAST_REGISTRY_USERNAME"), getenv("LAST_REGISTRY_PASSWORD"); u != "" || p != "" {
		creds = &images.Credentials{Username: u, Password: p}
	}
	insecure := strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.0.0.1:")
	return images.NewClient(host, creds, insecure).Push(ctx, repo, tag, img)
}

// splitDestination splits HOST/REPO:TAG.
func splitDestination(to string) (host, repo, tag string, err error) {
	host, rest, ok := strings.Cut(to, "/")
	if !ok || host == "" {
		return "", "", "", fmt.Errorf("--to %q must be HOST/REPO:TAG", to)
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 || strings.Contains(rest[i:], "/") {
		return "", "", "", fmt.Errorf("--to %q must be HOST/REPO:TAG", to)
	}
	return host, rest[:i], rest[i+1:], nil
}

func pull(ctx context.Context, ref, platform string) (v1.Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse --from %q: %w", ref, err)
	}
	p, err := v1.ParsePlatform(platform)
	if err != nil {
		return nil, fmt.Errorf("parse --platform %q: %w", platform, err)
	}
	img, err := remote.Image(r, remote.WithPlatform(*p), remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", ref, err)
	}
	return img, nil
}

func withLabel(img v1.Image, label string) (v1.Image, error) {
	k, v, ok := strings.Cut(label, "=")
	if !ok || k == "" {
		return nil, errors.New("--label must be KEY=VALUE")
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	cfg = cfg.DeepCopy()
	if cfg.Config.Labels == nil {
		cfg.Config.Labels = map[string]string{}
	}
	cfg.Config.Labels[k] = v
	out, err := mutate.Config(img, cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("set label: %w", err)
	}
	return out, nil
}
