package builder

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	go_boost_types "github.com/flashbots/go-boost-utils/types"
	"github.com/ralexstokes/relay-monitor/pkg/types"
)

type Client struct {
	endpoint string
	identity string
	client   http.Client
}

func (c *Client) String() string {
	return c.ID()
}

func (c *Client) ID() string {
	return c.identity
}

func NewClient(endpoint string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	publicKey := u.User.Username()

	client := http.Client{
		Timeout: 2 * time.Second,
	}
	return &Client{
		endpoint: endpoint,
		identity: publicKey,
		client:   client,
	}, nil
}

// `status` endpoint in the Builder API
func (c *Client) GetStatus() error {
	url := c.endpoint + "/eth/v1/builder/status"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay status was not healthy with HTTP status code %d", resp.StatusCode)
	}
	return nil
}

// `getHeader` endpoint in the Builder API
func (c *Client) GetBid(slot types.Slot, parentHash types.Hash, publicKey types.PublicKey) (*types.Bid, error) {
	url := c.endpoint + fmt.Sprintf("/eth/v1/builder/header/%d/%s/%s", slot, parentHash, publicKey)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get bid with HTTP status code %d", resp.StatusCode)
	}

	var bid go_boost_types.GetHeaderResponse
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&bid)
	return bid.Data, err
}
