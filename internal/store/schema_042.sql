-- zerg schema, version 42: a feature review is about a head, not a feature.
--
-- Issue #40, phase 4. The architect may reject a feature and may not approve
-- one. Its verdict is evidence on the operator's land, bound to the head sha
-- it looked at. Any later integration moves the head and the review is about
-- something that no longer exists — the class of bug ARCHITECTURE 6.1 collects.
--
-- Not an approvals row: those are tied to a message and a route, and a feature
-- land is tied to neither. Landing is a separate operator action; a verdict of
-- ok here is a recommendation, not a merge.
CREATE TABLE feature_reviews (
    id           TEXT PRIMARY KEY,
    feature_id   TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    head_sha     TEXT NOT NULL,
    verdict      TEXT NOT NULL CHECK (verdict IN ('ok', 'reject')),
    note         TEXT NOT NULL DEFAULT '',
    evidence_sha TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);

CREATE INDEX idx_feature_reviews_feature ON feature_reviews (feature_id, created_at);
