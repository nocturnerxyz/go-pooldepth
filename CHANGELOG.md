# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is `0`, the API may change in a minor release.

## [Unreleased]

### Added

- `DepthCurve` — tradeable size at several cost budgets in one ascending sweep,
  threading each level's answer forward as a floor for the next.
- `TotalDepth` — aggregate depth across several pools, including a mix of AMM types.

- `V2Pool` — constant-product pricing with a closed-form `DepthWithinBps`.
- `V3Pool` — concentrated liquidity with full tick traversal, liquidity-net
  crossing, and gap skipping.
- `Pool` interface satisfied by both, so callers need not branch on AMM type.
- `Quote` reporting execution cost (fees included) and price impact (fees
  excluded) separately, plus partial-fill reporting when liquidity runs out.
- `V3Pool.Validate` — reports whether the supplied ticks are a window or a
  complete pool.
- Exact integer tick math ported from `TickMath`/`SwapMath`, verified against
  the on-chain anchor values.
