package agentgateway

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/capsohq/bifrost/tests/agentgateway/driver"
	"github.com/capsohq/bifrost/tests/agentgateway/fixture"
)

// TestPushConfigTenantScopingLive checks, on the wire against a running gateway
// and the fixture agent, that a push configuration the gateway stores belongs to
// the tenant that registered it: another tenant naming the same task and config
// ID cannot replace, read, list or delete it, and a request naming no tenant is
// the empty tenant rather than a wildcard. Same environment as TestLifecycle.
func TestPushConfigTenantScopingLive(t *testing.T) {
	gwURL := os.Getenv("AGENTGATEWAY_URL")
	if gwURL == "" {
		t.Skip("AGENTGATEWAY_URL is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	token := os.Getenv("AGENTGATEWAY_ADMIN_TOKEN")
	if user := os.Getenv("AGENTGATEWAY_ADMIN_USER"); token == "" && user != "" {
		var err error
		if token, err = driver.Login(ctx, gwURL, user, os.Getenv("AGENTGATEWAY_ADMIN_PASS")); err != nil {
			t.Fatal(err)
		}
	}
	gw := driver.NewGateway(gwURL, token)
	bind, host := envOr("AGENTGATEWAY_BIND", "127.0.0.1")+":0", envOr("AGENTGATEWAY_HOST", "127.0.0.1")

	fx, err := fixture.Start(bind, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fx.Close() })
	sink, err := driver.StartSink(bind, host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	agent := fmt.Sprintf("push-tenancy-fixture-%d", time.Now().UnixNano())
	if err := gw.RegisterAgent(ctx, agent, fx.CardURL, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := gw.DeleteAgent(c, agent); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	vk := os.Getenv("AGENTGATEWAY_VK")
	card, err := gw.WaitForCard(ctx, agent, vk, "")
	if err != nil {
		t.Fatal(err)
	}
	var options []a2aclient.FactoryOption
	if vk != "" {
		options = append(options, a2aclient.WithCallInterceptors(securityHeader{key: vk}))
	}
	client, err := a2aclient.NewFromCard(ctx, card, options...)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Destroy()

	// A task to hang the configurations on.
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("start lifecycle"))
	first, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := first.(*a2a.Task)
	if !ok {
		t.Fatalf("send returned %T, want task", first)
	}
	const configID = "shared-config"

	if _, err := client.CreateTaskPushConfig(ctx, &a2a.PushConfig{Tenant: "tenant-a", TaskID: task.ID, ID: configID, URL: sink.URL, Token: sink.Token}); err != nil {
		t.Fatalf("tenant-a create: %v", err)
	}
	if _, err := client.CreateTaskPushConfig(ctx, &a2a.PushConfig{Tenant: "tenant-b", TaskID: task.ID, ID: configID, URL: sink.URL, Token: "intruder"}); err == nil {
		t.Fatal("tenant-b registered the config ID tenant-a holds for the same task: the callback was replaced")
	}
	if _, err := client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: "tenant-b", TaskID: task.ID, ID: configID}); err == nil {
		t.Fatal("tenant-b read tenant-a's push configuration")
	}
	if _, err := client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{TaskID: task.ID, ID: configID}); err == nil {
		t.Fatal("a tenantless read saw tenant-a's push configuration")
	}
	for _, tenant := range []string{"tenant-b", ""} {
		configs, err := client.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{Tenant: tenant, TaskID: task.ID})
		if err != nil {
			t.Fatalf("list as %q: %v", tenant, err)
		}
		if len(configs) != 0 {
			t.Fatalf("list as %q returned %d of tenant-a's configurations", tenant, len(configs))
		}
	}
	if err := client.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{Tenant: "tenant-b", TaskID: task.ID, ID: configID}); err != nil {
		t.Fatalf("tenant-b delete: %v", err)
	}
	got, err := client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: "tenant-a", TaskID: task.ID, ID: configID})
	if err != nil {
		t.Fatalf("tenant-a read after tenant-b's delete: %v (tenant-b removed tenant-a's callback record)", err)
	}
	if got.URL != sink.URL || got.Tenant != "tenant-a" {
		t.Fatalf("tenant-a's configuration is url=%q tenant=%q, want url=%q tenant=tenant-a", got.URL, got.Tenant, sink.URL)
	}
	configs, err := client.ListTaskPushConfigs(ctx, &a2a.ListTaskPushConfigRequest{Tenant: "tenant-a", TaskID: task.ID})
	if err != nil || len(configs) != 1 {
		t.Fatalf("tenant-a list: count=%d error=%v", len(configs), err)
	}
	if err := client.DeleteTaskPushConfig(ctx, &a2a.DeleteTaskPushConfigRequest{Tenant: "tenant-a", TaskID: task.ID, ID: configID}); err != nil {
		t.Fatalf("tenant-a delete: %v", err)
	}
	if _, err := client.GetTaskPushConfig(ctx, &a2a.GetTaskPushConfigRequest{Tenant: "tenant-a", TaskID: task.ID, ID: configID}); err == nil {
		t.Fatal("tenant-a's configuration survived its own delete")
	}
}
