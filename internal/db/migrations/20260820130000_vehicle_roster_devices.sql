-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

-- The roster learns which device makes a vehicle connected.
--
-- Plan 07 step 4. Step 3 gave the roster owner, definition and mint time —
-- enough to answer "what is this vehicle", and NOT enough to replace what
-- fleet-lite renders. Its fleet list shows a connection indicator driven by
-- `syntheticDevice.tokenId > 0` / `aftermarketDevice.tokenId > 0`
-- (web/src/views/fleet-list-view.ts), and its detail view names the device by
-- token id. A cutover to a roster without these columns would have blanked
-- that indicator for every vehicle — a silent visual regression on exactly the
-- page the cutover is for.
--
-- Both are the chain's answer, like owner, so they are RE-READ AND OVERWRITTEN
-- on every reconcile rather than filled forward the way VIN and plate are. The
-- difference is which service is the source: identity-api serves these and does
-- not serve VIN or plate, so a NULL here means the chain says "no device",
-- which is a fact worth recording. Unpairing a device is a real event and the
-- roster must be able to show it, or it would report a vehicle as connected
-- forever after its device was removed.
ALTER TABLE vehicles
    ADD COLUMN IF NOT EXISTS synthetic_device_token_id  BIGINT,
    ADD COLUMN IF NOT EXISTS aftermarket_device_token_id BIGINT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

ALTER TABLE vehicles
    DROP COLUMN IF EXISTS synthetic_device_token_id,
    DROP COLUMN IF EXISTS aftermarket_device_token_id;

-- +goose StatementEnd
