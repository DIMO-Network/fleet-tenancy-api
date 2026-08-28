package sharing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/DIMO-Network/go-zerodev/account"
	"github.com/DIMO-Network/go-zerodev/types"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
)

// The SACD document — the JSON a share points at through its on-chain `source`.
//
// Permission bits on chain say what an app may READ. They say nothing about
// documents: dimo-app-backend gates a grantee's glovebox access on a
// `cloudevent` agreement inside this document, and a doc-only share is valid
// with permissions == 0. A share whose source is empty therefore grants
// telemetry and no documents, which is what ours did.
//
// The shape below is the DIMO Transactions SDK's, byte for byte — see
// generatePermissionsSACDTemplate in @dimo-network/transactions
// (core/actions/setPermissionsSACD.js). fleet-pairing produces it through the
// SDK; we are Go, so we build it directly. If the SDK's shape moves, this
// moves with it: consumers parse these field names exactly.

// docEventTypePatterns are the event types a full share grants, matching
// fleet-pairing's DOC_EVENT_TYPE_PATTERNS and dimo-app-backend's
// matchesDocumentEventType. The trailing `*` is a prefix match, not a glob.
var docEventTypePatterns = []string{
	"dimo.document.vehicle.*",
	"dimo.document.driver.*",
	"dimo.raw.vehicle.*",
	"dimo.raw.driver.*",
}

// sdkPermissionNames maps our Permission to the SDK's enum key, which is what
// lands in the document as `privilege:<key>`. The two vocabularies differ —
// ours is named for the data, the SDK's for the operation — and pairing them
// wrongly writes a document that grants something other than what the on-chain
// mask does. Taken from fleet-pairing's convertToPermissionArray.
var sdkPermissionNames = map[Permission]string{
	NonLocationTelemetry: "GetNonLocationHistory",
	Commands:             "ExecuteCommands",
	CurrentLocation:      "GetCurrentLocation",
	AllTimeLocation:      "GetLocationHistory",
	Credentials:          "GetVINCredential",
	Streams:              "GetLiveData",
	RawData:              "GetRawData",
	ApproximateLocation:  "GetApproximateLocation",
}

type sacdParty struct {
	Address string `json:"address"`
}

type sacdPermissionName struct {
	Name string `json:"name"`
}

// sacdAgreement is both agreement kinds. `type` discriminates: a "permission"
// agreement carries Permissions/Attachments/Extensions, a "cloudevent" one
// carries EventType/Source/IDs/Tags. Fields absent from a kind are omitted
// rather than emitted empty, matching the SDK's two object literals.
type sacdAgreement struct {
	Type        string               `json:"type"`
	Asset       string               `json:"asset"`
	Permissions []sacdPermissionName `json:"permissions,omitempty"`
	Attachments []string             `json:"attachments,omitempty"`
	Extensions  map[string]any       `json:"extensions,omitempty"`
	EventType   string               `json:"eventType,omitempty"`
	Source      string               `json:"source,omitempty"`
	IDs         []string             `json:"ids,omitempty"`
	Tags        []string             `json:"tags,omitempty"`
	EffectiveAt string               `json:"effectiveAt,omitempty"`
	ExpiresAt   string               `json:"expiresAt,omitempty"`
}

type sacdData struct {
	Grantor         sacdParty       `json:"grantor"`
	Grantee         sacdParty       `json:"grantee"`
	EffectiveAt     string          `json:"effectiveAt"`
	ExpiresAt       string          `json:"expiresAt"`
	AdditionalDates map[string]any  `json:"additionalDates"`
	Agreements      []sacdAgreement `json:"agreements"`
}

// SACDDocument is the uploaded artifact. Field order matters only for
// readability; consumers read by name.
type SACDDocument struct {
	// `specversion`, lower case. The DIMO SACD spec and its examples use that
	// casing, and cloudevent.RawEvent — the struct every Go reader unmarshals
	// these documents into — tags it `json:"specversion"`. The TS SDK emits
	// `specVersion`, which silently leaves the field empty on the read side.
	// Nothing checks it today; matching the spec costs nothing and removes a
	// trap for whoever starts checking.
	SpecVersion string   `json:"specversion"`
	Time        string   `json:"time"`
	Type        string   `json:"type"`
	DataVersion string   `json:"dataversion"`
	Data        sacdData `json:"data"`
	Signature   string   `json:"signature"`
}

// VehicleAssetDID is the asset a SACD names. It must be the DID, not the NFT
// contract address — the on-chain call takes the contract, the document takes
// the DID, and dimo-app-backend compares this string against the vehicle DID it
// builds itself. The address is checksummed via common.Address.Hex().
func VehicleAssetDID(chainID int64, vehicleNft common.Address, tokenID int64) string {
	return fmt.Sprintf("did:erc721:%d:%s:%d", chainID, vehicleNft.Hex(), tokenID)
}

// BuildSACDDocument assembles the document for one share. Pure and
// network-free so the exact bytes are unit-testable — this is what a grantee's
// document access is decided from, and it cannot be inspected after upload
// without fetching it back.
//
// `signature` is left as the SDK's "0x" placeholder; SignSACDDocument fills it.
func BuildSACDDocument(
	grantor, grantee common.Address,
	asset string,
	permissions []Permission,
	now time.Time,
	expiration *big.Int,
	shareDocuments bool,
) SACDDocument {
	effectiveAt := now.UTC().Format(time.RFC3339)
	expiresAt := time.Unix(expiration.Int64(), 0).UTC().Format(time.RFC3339)

	names := make([]sacdPermissionName, 0, len(permissions))
	for _, p := range permissions {
		if name, ok := sdkPermissionNames[p]; ok {
			names = append(names, sacdPermissionName{Name: "privilege:" + name})
		}
	}

	agreements := []sacdAgreement{{
		Type:        "permission",
		Asset:       asset,
		Permissions: names,
		Attachments: []string{},
		Extensions:  map[string]any{},
	}}

	if shareDocuments {
		for _, eventType := range docEventTypePatterns {
			agreements = append(agreements, sacdAgreement{
				Type:      "cloudevent",
				EventType: eventType,
				// The grantor's address: whose documents are being shared.
				Source:      grantor.Hex(),
				Asset:       asset,
				IDs:         []string{},
				Tags:        []string{"documents"},
				EffectiveAt: effectiveAt,
				ExpiresAt:   expiresAt,
			})
		}
	}

	return SACDDocument{
		SpecVersion: "1.0",
		Time:        effectiveAt,
		Type:        "dimo.sacd",
		DataVersion: "sacd/v1.0",
		Data: sacdData{
			Grantor:         sacdParty{Address: grantor.Hex()},
			Grantee:         sacdParty{Address: grantee.Hex()},
			EffectiveAt:     effectiveAt,
			ExpiresAt:       expiresAt,
			AdditionalDates: map[string]any{},
			Agreements:      agreements,
		},
		Signature: "0x",
	}
}

// SigningPayload is the exact bytes the signature must cover: the marshalled
// `data` object, and nothing else.
//
// This is token-exchange's contract, not a choice. It fetches the document,
// unmarshals it into a cloudevent.RawEvent, and checks
// `ValidateSignature(record.Data, record.Signature, data.Grantor.Address)` —
// so the payload is the raw `data` sub-object as it appears on the wire
// (services/access/access.go, services/ipfs_service.go).
//
// This deliberately differs from @dimo-network/transactions, whose
// signSACDPermissionTemplate signs JSON.stringify of the WHOLE template
// (verified in fleet-pairing's installed v0.3.2). A signature over the whole
// template cannot validate against `record.Data`, so SDK-produced documents
// appear to fail this check — which would mean SDK-based shares grant
// telemetry but not documents. Follow the verifier, not the producer.
//
// Getting this wrong is survivable in one direction only. ValidateAccess falls
// back to the on-chain bitmask when the source doc is unusable, so a bad
// signature costs document access and nothing else — EXCEPT for requests
// carrying EventFilters, which return the source-doc error outright
// (services/access/access.go). Those are exactly the document requests this
// change exists to make work.
func (d SACDDocument) SigningPayload() ([]byte, error) {
	return json.Marshal(d.Data)
}

// UploadSACDDocument posts the signed document and returns its CID.
//
// The endpoint pins the JSON and answers with a bare CID; the on-chain `source`
// needs the `ipfs://` scheme in front of it, which SourceURI adds. Getting that
// wrong is silent: the grant lands, and every reader fails to resolve it.
func UploadSACDDocument(ctx context.Context, uploadURL string, doc SACDDocument) (string, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal SACD document: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build SACD upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("upload SACD document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("SACD upload returned %d", resp.StatusCode)
	}

	var result struct {
		CID string `json:"cid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode SACD upload response: %w", err)
	}
	if result.CID == "" {
		return "", fmt.Errorf("SACD upload returned no cid")
	}
	return result.CID, nil
}

// SourceURI is what goes on chain for a given CID.
func SourceURI(cid string) string { return "ipfs://" + cid }

// SignSACDDocument signs the document's `data` object AS THE GRANTOR and
// returns it ready to upload.
//
// The grantor is the vehicle owner's kernel account, and the owner never
// signs — that is the premise of this service. What makes this work is that
// the kernel has the acting tenant's signer installed as a secondary
// validator, so the kernel will vouch for that key's signatures through
// ERC-1271. go-zerodev's SmartAccountPrivateKeySigner produces exactly that
// signature: it wraps the hash in the kernel's EIP-712 domain
// (`Kernel(bytes32 hash)`) and prepends the validator identifier.
//
// The hash must be `accounts.TextHash(payload)` — the EIP-191 personal-sign
// hash — because that is what token-exchange passes to isValidSignature
// (signature/validator.go). SignMessage is deliberately NOT used: it applies a
// plain Keccak256 with no EIP-191 prefix, which the verifier would never
// reproduce.
//
// Needs an RPC client: the kernel's EIP-712 domain is read from the account
// on chain.
func SignSACDDocument(
	ctx context.Context,
	rpcClient types.RPCClient,
	doc SACDDocument,
	grantor common.Address,
	signerPK *ecdsa.PrivateKey,
) (SACDDocument, error) {
	payload, err := doc.SigningPayload()
	if err != nil {
		return doc, fmt.Errorf("serialize SACD data for signing: %w", err)
	}

	// Signed by the kernel (the grantor), not by the key holding the pen.
	kernelSigner, err := account.NewSmartAccountPrivateKeySigner(rpcClient, grantor, signerPK)
	if err != nil {
		return doc, fmt.Errorf("build kernel signer for %s: %w", grantor.Hex(), err)
	}

	sig, err := kernelSigner.SignHash(common.BytesToHash(accounts.TextHash(payload)))
	if err != nil {
		return doc, fmt.Errorf("kernel-sign SACD data: %w", err)
	}

	doc.Signature = "0x" + hex.EncodeToString(sig)
	return doc, nil
}
