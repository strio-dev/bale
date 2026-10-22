package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strio-dev/bale"
)

func TestAllPublicationMethodsWaitAndCancelTogether(t *testing.T) {
	for _, operation := range []string{"execute", "create", "push"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			registry := newSDKRegistry()
			server := httptest.NewServer(registry)
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "http://")
			held, release := make(chan struct{}), make(chan struct{})
			var holdOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}, OnProgress: func(p bale.Progress) {
				if p.Operation == "attachment-published" {
					holdOnce.Do(func() { close(held); <-release })
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			subject := attachmentSubject(t, client, host)
			recipe := bale.Recipe{Include: []bale.Selection{{Ref: subject.Repository + ":a", Referrers: bale.ReferrerPolicy{Include: "none"}}}}
			bundleRef := host + "/bundles:complete"
			created, err := client.Create(ctx, recipe, bundleRef, bale.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			layout := filepath.Join(t.TempDir(), "layout")
			if _, err := client.Pull(ctx, bundleRef, layout, bale.PullLocked); err != nil {
				t.Fatal(err)
			}
			pushPlan, err := client.PlanPush(ctx, layout, host+"/delivery:complete", bale.PushOptions{})
			if err != nil {
				t.Fatal(err)
			}
			local := attachmentFixture(t, nil, false, "new publication")
			executePlan, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{{Repository: "offline.example/component", Descriptor: local.Descriptor, Store: local.Store, Aliases: []string{"new"}, Referrers: bale.ReferrerPolicy{Include: "none"}}}, Destination: host + "/direct"})
			if err != nil {
				t.Fatal(err)
			}
			// The fake registry shares digest objects across repositories. Remove
			// only the stored bundle root so Push/Create must write it afresh.
			registry.mu.Lock()
			delete(registry.objects, created.Descriptor.Digest.String())
			registry.mu.Unlock()
			attachment := attachmentFixture(t, &subject.Descriptor, true, "holding publication")
			attachment.Subject = subject.Repository + ":a"
			attachDone := make(chan error, 1)
			go func() { _, err := client.Attach(ctx, attachment); attachDone <- err }()
			select {
			case <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("Attach did not reach publication")
			}
			registry.mu.Lock()
			before := registry.writes
			registry.mu.Unlock()
			waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "execute":
					_, err = client.Execute(waitCtx, executePlan)
				case "create":
					_, err = client.Create(waitCtx, recipe, host+"/bundles:new", bale.CreateOptions{})
				case "push":
					_, err = client.Push(waitCtx, pushPlan)
				}
				done <- err
			}()
			// Planning and dry-run capture remain available while a writer holds
			// the permit; neither performs registry publication.
			if _, err := client.Plan(ctx, bale.TransferRequest{Roots: []bale.Root{{Repository: "offline.example/component", Descriptor: local.Descriptor, Store: local.Store, Referrers: bale.ReferrerPolicy{Include: "none"}}}, Destination: host + "/inspect"}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Create(ctx, recipe, host+"/bundles:dry", bale.CreateOptions{DryRun: true}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("waiting %s lost cancellation: %v", operation, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("waiting %s could not cancel", operation)
			}
			registry.mu.Lock()
			after := registry.writes
			registry.mu.Unlock()
			if after != before {
				t.Errorf("waiting %s wrote %d requests", operation, after-before)
			}
			unblock()
			if err := <-attachDone; err != nil {
				t.Fatal(err)
			}
			// A canceled waiter must not consume or leak the publication permit.
			switch operation {
			case "execute":
				_, err = client.Execute(ctx, executePlan)
			case "create":
				_, err = client.Create(ctx, recipe, host+"/bundles:new", bale.CreateOptions{})
			case "push":
				_, err = client.Push(ctx, pushPlan)
			}
			if err != nil {
				t.Fatalf("retry %s: %v", operation, err)
			}
		})
	}
}
