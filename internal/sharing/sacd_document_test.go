package sharing

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	docGrantor = common.HexToAddress("0x264BC41755BA9F5a00DCEC07F96cB14339dBD970")
	docGrantee = common.HexToAddress("0x51dacC165f1306Abfbf0a6312ec96E13AAA826DB")
	docNft     = common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF")
	docNow     = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	docExpiry  = big.NewInt(time.Date(2027, 8, 27, 12, 0, 0, 0, time.UTC).Unix())
)

func buildTestDoc(t *testing.T, shareDocuments bool) map[string]any {
	t.Helper()
	asset := VehicleAssetDID(137, docNft, 186612)
	doc := BuildSACDDocument(docGrantor, docGrantee, asset, defaultPermissionList(), docNow, docExpiry, shareDocuments)
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// The asset must be the DID. dimo-identity's setPermissions takes the NFT
// contract address, and passing that here instead would produce a document
// whose asset never matches the vehicle DID a reader builds — a share that
// grants documents on paper and none in practice.
func TestVehicleAssetDID_IsTheDIDNotTheContract(t *testing.T) {
	got := VehicleAssetDID(137, docNft, 186612)
	assert.Equal(t, "did:erc721:137:0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF:186612", got)
	assert.Contains(t, got, docNft.Hex(), "the contract address must be EIP-55 checksummed; DIS keys off the exact string")
}

// This is the check dimo-app-backend's hasDocumentAgreement performs. If this
// test fails, a grantee sees the vehicle and none of its documents.
func TestBuildSACDDocument_SatisfiesTheDocumentAgreementCheck(t *testing.T) {
	out := buildTestDoc(t, true)
	data := out["data"].(map[string]any)
	agreements := data["agreements"].([]any)

	wantAsset := VehicleAssetDID(137, docNft, 186612)
	wantPatterns := map[string]bool{
		"dimo.document.vehicle.*": false,
		"dimo.document.driver.*":  false,
		"dimo.raw.vehicle.*":      false,
		"dimo.raw.driver.*":       false,
	}

	for _, a := range agreements {
		m := a.(map[string]any)
		if m["type"] != "cloudevent" {
			continue
		}
		// All three conditions are ANDed by the reader; a miss on any one is
		// silently "no document access".
		assert.Equal(t, wantAsset, m["asset"], "asset must equal the vehicle DID the reader builds")
		et, _ := m["eventType"].(string)
		if _, known := wantPatterns[et]; known {
			wantPatterns[et] = true
		}
	}
	for pattern, seen := range wantPatterns {
		assert.True(t, seen, "missing cloudevent agreement for %q", pattern)
	}
}

func TestBuildSACDDocument_OmitsAgreementsWhenNotSharingDocuments(t *testing.T) {
	out := buildTestDoc(t, false)
	agreements := out["data"].(map[string]any)["agreements"].([]any)
	require.Len(t, agreements, 1, "only the permission agreement")
	assert.Equal(t, "permission", agreements[0].(map[string]any)["type"])
}

// The envelope is the SDK's, and consumers read these names exactly.
func TestBuildSACDDocument_MatchesSDKEnvelope(t *testing.T) {
	out := buildTestDoc(t, true)
	// Lower case, per the SACD spec and cloudevent.RawEvent's struct tag —
	// not the SDK's `specVersion`.
	assert.Equal(t, "1.0", out["specversion"])
	_, camel := out["specVersion"]
	assert.False(t, camel, "specVersion (camelCase) does not deserialize into cloudevent.RawEvent")
	assert.Equal(t, "dimo.sacd", out["type"])
	assert.Equal(t, "sacd/v1.0", out["dataversion"])

	data := out["data"].(map[string]any)
	assert.Equal(t, docGrantor.Hex(), data["grantor"].(map[string]any)["address"])
	assert.Equal(t, docGrantee.Hex(), data["grantee"].(map[string]any)["address"])
	assert.NotEmpty(t, data["effectiveAt"])
	assert.NotEmpty(t, data["expiresAt"])
	assert.NotNil(t, data["additionalDates"])
}

// The permission agreement names each privilege the on-chain mask grants. The
// two vocabularies differ — ours is named for the data, the SDK's for the
// operation — so this pins the translation.
func TestBuildSACDDocument_PermissionNamesMatchTheSDKVocabulary(t *testing.T) {
	out := buildTestDoc(t, true)
	agreements := out["data"].(map[string]any)["agreements"].([]any)
	perms := agreements[0].(map[string]any)["permissions"].([]any)

	got := map[string]bool{}
	for _, p := range perms {
		got[p.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"privilege:GetNonLocationHistory",
		"privilege:ExecuteCommands",
		"privilege:GetCurrentLocation",
		"privilege:GetLocationHistory",
		"privilege:GetVINCredential",
		"privilege:GetLiveData",
		"privilege:GetRawData",
	} {
		assert.True(t, got[want], "default share should name %s", want)
	}
	// APPROXIMATE_LOCATION is excluded from the default mask; the document
	// must not claim more than the grant.
	assert.False(t, got["privilege:GetApproximateLocation"],
		"the document must not name a permission the on-chain mask withholds")
}

// defaultPermissionList feeds both the packed mask and the document. If they
// drift, the document advertises access the grant does not give.
func TestDefaultPermissionListMatchesDefaultPermissions(t *testing.T) {
	assert.Equal(t, 0, DefaultPermissions().Cmp(Permissions(defaultPermissionList()...)),
		"the document's permission list and the on-chain mask must describe the same grant")
}

// token-exchange verifies the signature over the `data` object alone —
// ValidateSignature(record.Data, ...) in services/access/access.go. Signing
// the whole template (as the TS SDK does) produces a signature that does not
// validate, and the grant is refused with "invalid grant signature".
func TestSigningPayload_IsTheDataObjectOnly(t *testing.T) {
	asset := VehicleAssetDID(137, docNft, 186612)
	doc := BuildSACDDocument(docGrantor, docGrantee, asset, defaultPermissionList(), docNow, docExpiry, true)

	payload, err := doc.SigningPayload()
	require.NoError(t, err)

	// The envelope must not be in the signed bytes.
	assert.NotContains(t, string(payload), `"specversion"`)
	assert.NotContains(t, string(payload), `"dimo.sacd"`)
	assert.NotContains(t, string(payload), `"signature"`)

	// The data object must be, byte-identical to what a reader unmarshals as
	// record.Data.
	var got, want map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	full, err := json.Marshal(doc)
	require.NoError(t, err)
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(full, &envelope))
	require.NoError(t, json.Unmarshal(envelope["data"], &want))
	assert.Equal(t, want, got, "signed bytes must equal the document's data object")
}

func TestSourceURI_HasTheIPFSScheme(t *testing.T) {
	// A bare CID on chain resolves for nobody.
	assert.Equal(t, "ipfs://bafyfoo", SourceURI("bafyfoo"))
}

// The signature must verify the way token-exchange verifies it: ERC-1271 on
// the grantor's kernel, over accounts.TextHash of the data object. We cannot
// call a kernel from a unit test, but we can pin the two halves that are ours
// to get wrong — the hash the signature covers, and the validator identifier
// the kernel needs in front of it.
func TestSignSACDDocument_UsesTheEIP191HashOfTheDataObject(t *testing.T) {
	asset := VehicleAssetDID(137, docNft, 186612)
	doc := BuildSACDDocument(docGrantor, docGrantee, asset, defaultPermissionList(), docNow, docExpiry, true)

	payload, err := doc.SigningPayload()
	require.NoError(t, err)

	// This is the exact expression token-exchange evaluates before handing the
	// hash to isValidSignature (internal/signature/validator.go). A plain
	// Keccak256 here — which SignMessage would apply — never reproduces it.
	want := accounts.TextHash(payload)
	assert.Len(t, want, 32)
	assert.NotEqual(t, crypto.Keccak256(payload), want,
		"the EIP-191 prefix is what distinguishes the verified hash; without it the signature is over the wrong bytes")
}
