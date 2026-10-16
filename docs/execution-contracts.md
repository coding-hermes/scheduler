# Execution contract schemas

The public execution-plane contracts are JSON Schema Draft 2020-12 resources
under `schemas/execution/v1`. Version `1.0` is carried in every document. These
contracts define planning and evidence messages only; they do not activate the
new execution plane, and the existing `work.dispatch` lane contract is unchanged.

Within major version 1, producers may make only changes that remain valid under
the published closed schemas (for example, documentation clarifications). A new
optional or required member, a vocabulary change, or changed semantics requires
a new schema version. Consumers must reject unsupported versions and unknown
members rather than guessing. Producers and consumers should be upgraded with a
period where both explicitly supported versions are available.

All objects are closed with `additionalProperties: false`. This prevents ambient
credentials, private reasoning, raw environment, and untyped metadata from
entering the contracts by shape. Artifact bytes remain out of band and are
represented by URI, size, media type, and SHA-256 references.

ExecutionBrief digests are computed by the normative NFC + RFC 8785 procedure in
`internal/execution`. The checked-in vector file is intended for implementations
in any language: canonical bytes and SHA-256 values are authorities, while agent
prose is not. Explicit JSON `null` remains present and therefore differs from an
absent member.
