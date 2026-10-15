package postcheckout_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"livecart/apps/api/internal/postcheckout"
	"livecart/apps/api/lib/storage"
)

func TestTrackingPresignsPrivateStoreLogo(t *testing.T) {
	requireDB(t)
	for key, value := range map[string]string{"AWS_ACCESS_KEY_ID": "local-test", "AWS_SECRET_ACCESS_KEY": "local-test-secret", "AWS_REGION": "us-east-1", "S3_BUCKET": "test-bucket", "S3_ENDPOINT": "https://storage.example.test", "CDN_BASE_URL": ""} {
		t.Setenv(key, value)
	}
	s3, err := storage.NewS3Client()
	if err != nil {
		t.Fatal(err)
	}
	cartID := seedPaidCartWithOrder(t)
	if _, err := testPool.Exec(t.Context(), `UPDATE stores SET logo_url='stores/test/logo.png' WHERE id=(SELECT e.store_id FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1)`, cartID); err != nil {
		t.Fatal(err)
	}
	var shortID int
	if err := testPool.QueryRow(t.Context(), `SELECT short_id FROM carts WHERE id=$1`, cartID).Scan(&shortID); err != nil {
		t.Fatal(err)
	}
	token, ok := logisticsToken(t, t.Context(), cartID)
	if !ok {
		t.Fatal("missing token")
	}
	handler := postcheckout.NewHandler(postcheckout.NewRepository(testQueries), newService(t), testPool, zap.NewNop(), s3)
	app := fiber.New()
	handler.RegisterPublicRoutes(app)
	response, err := app.Test(httptest.NewRequest("GET", fmt.Sprintf("/api/public/orders/%d?key=%s", shortID, url.QueryEscape(token)), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var body struct {
		Data postcheckout.PublicOrderResponse `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	logo, err := url.Parse(body.Data.StoreLogoURL)
	if err != nil {
		t.Fatal(err)
	}
	if logo.Host != "storage.example.test" || logo.Query().Get("X-Amz-Signature") == "" || logo.Path != "/test-bucket/stores/test/logo.png" {
		t.Fatalf("logo is not signed: %s", body.Data.StoreLogoURL)
	}
}
