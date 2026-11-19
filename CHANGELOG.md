# Changelog

## [0.3.0](https://github.com/mctlhq/mctl-glovo/compare/0.2.0...0.3.0) (2026-11-19)


### Features

* **c2:** user and operator unlink and erase routes ([fb8638a](https://github.com/mctlhq/mctl-glovo/commit/fb8638a0947d32bef4c484c82868efb4fa35e393))
* **c2:** user and operator unlink and erase routes ([88dd07f](https://github.com/mctlhq/mctl-glovo/commit/88dd07f038f477466c7d38b2808e61c9839b2ad5))


### Bug Fixes

* **c2:** record the operator as the actor of an operator unlink ([58fccc4](https://github.com/mctlhq/mctl-glovo/commit/58fccc43791a3d469294df14ef1fcf9cbeb4d09f))
* **c2:** record the operator as the actor of an operator unlink ([a6c462c](https://github.com/mctlhq/mctl-glovo/commit/a6c462c9828e165b2aa104382901fb25b6fc8c0e))

## [0.2.0](https://github.com/mctlhq/mctl-glovo/compare/0.1.0...0.2.0) (2026-11-19)


### Features

* **c2:** fresh MCTL login and pid-bound code on the link routes ([#42](https://github.com/mctlhq/mctl-glovo/issues/42)) ([6558247](https://github.com/mctlhq/mctl-glovo/commit/65582474f878b29011f83d6604e4ac8e34340e4e))
* **c2:** fresh MCTL login and pid-bound code on the link routes ([#42](https://github.com/mctlhq/mctl-glovo/issues/42)) ([c117951](https://github.com/mctlhq/mctl-glovo/commit/c1179519398bedd6c860c1e539c2749ad06c5a9a))
* **c2:** read-only hosted C2 by default ([aa9b3cc](https://github.com/mctlhq/mctl-glovo/commit/aa9b3cc39cc7a8bbc834f04dfe841868a3ec65d6))
* **c2:** read-only hosted C2 by default ([97b437b](https://github.com/mctlhq/mctl-glovo/commit/97b437b752337131c24d51309eb62748ec29b8e1))
* **c2:** refuse GLOVO_C2_BASKET_WRITES, align consent and text with read-only ([187ca98](https://github.com/mctlhq/mctl-glovo/commit/187ca98aa168c5d8860765f00572b4890bf7f65a))
* **glovo-link:** loopback MCTL login with PKCE for the link routes ([#42](https://github.com/mctlhq/mctl-glovo/issues/42) B2) ([43b0385](https://github.com/mctlhq/mctl-glovo/commit/43b03854ba20348e1533291823c7918decc71d82))
* **glovo-link:** loopback MCTL login with PKCE for the link routes ([#42](https://github.com/mctlhq/mctl-glovo/issues/42) B2) ([5f282f8](https://github.com/mctlhq/mctl-glovo/commit/5f282f87109379fc1f32f135cb0a9d8f3898940d))


### Bug Fixes

* **glovo-link:** annotate the constants gosec actually flags ([24e4349](https://github.com/mctlhq/mctl-glovo/commit/24e4349df13a9077c930804938e96a0945160d69))
* **glovo-link:** reap the browser opener and silence the last G101 ([ab6632a](https://github.com/mctlhq/mctl-glovo/commit/ab6632aed67f18d469a480823dfbf687b4fb6de8))
* **glovo-link:** tolerate clock skew and never be stricter than the server ([e54fc2c](https://github.com/mctlhq/mctl-glovo/commit/e54fc2ca0ee419eb523ed31de1a437ec90e2170c))
* **glovologic:** honest order-history analysis for "most ordered dish" ([#45](https://github.com/mctlhq/mctl-glovo/issues/45)) ([a3af531](https://github.com/mctlhq/mctl-glovo/commit/a3af531c897aab8eed22fe4532f77dcdf56b0f6c))
* **glovologic:** honest order-history analysis for most ordered dish ([#45](https://github.com/mctlhq/mctl-glovo/issues/45)) ([167f326](https://github.com/mctlhq/mctl-glovo/commit/167f326fb4029f5f5923fc8edcc9ec51b1e98b84))

## 0.1.0 (2026-11-17)


### Features

* build infra for C2 (Dockerfile, glovo-migrate, identmask) ([13f03a7](https://github.com/mctlhq/mctl-glovo/commit/13f03a76718c7596465e2fe348ceba6259d3f0a1))
* build infra for C2 (Dockerfile, glovo-migrate, identmask) ([1d61b24](https://github.com/mctlhq/mctl-glovo/commit/1d61b24a7d98e93578b47403949e368ce377339c))
* **c2:** configure token-store caps from env for a 10-20 user cohort ([d4ca7cd](https://github.com/mctlhq/mctl-glovo/commit/d4ca7cd4d63ed0e2fc64420af55a6f40a8833d98))
* **c2:** configure token-store caps from env for a 10-20 user cohort ([c7ee20b](https://github.com/mctlhq/mctl-glovo/commit/c7ee20be21fe8c84099b5330a93e22ee92044699))
* **c2:** kill switch flip endpoint and per-call enforcement ([657a30e](https://github.com/mctlhq/mctl-glovo/commit/657a30ec5ea4e369c5e6076792c8695c6e047d81))
* **c2:** kill switch flip endpoint and per-call enforcement (slice 5) ([67c27db](https://github.com/mctlhq/mctl-glovo/commit/67c27dbb2f01d1efb3a2315857d014a84666ee65))
* **c2:** per-call mctl-api consent admission (slice 3) ([290b954](https://github.com/mctlhq/mctl-glovo/commit/290b954705b073b99bd54c0ea5310b95c832c513))
* **c2:** per-call mctl-api consent admission for hosted tool calls ([545f3ca](https://github.com/mctlhq/mctl-glovo/commit/545f3caad051876dc3fb6c7face9c0c704656bba))
* **glovotools:** hosted location writes and a pinned no-checkout tool set ([145847b](https://github.com/mctlhq/mctl-glovo/commit/145847bcf5277051d3295798ecba8360b77b8faf))
* **glovotools:** hosted location writes and pinned no-checkout tool set ([dbde739](https://github.com/mctlhq/mctl-glovo/commit/dbde7396d51b1c0aeec9a571b11fb26b10f72e33))
* **landing:** adopt the MCTL public-site shell, head metadata and a11y fixes ([91e0576](https://github.com/mctlhq/mctl-glovo/commit/91e0576b72823344114fd020ed931252d5c50db3))
* **landing:** adopt the MCTL public-site shell, head metadata, a11y ([06101e4](https://github.com/mctlhq/mctl-glovo/commit/06101e47dff4763e3862550da3eb47ca02073eb2))
* **landing:** richer layout with Glovo-flavoured colours and MCTL accent ([fd7db6a](https://github.com/mctlhq/mctl-glovo/commit/fd7db6acb539779aaf7a824cabc3881e402a2c5a))
* **landing:** richer layout with Glovo-flavoured colours and the MCTL accent ([377ad7b](https://github.com/mctlhq/mctl-glovo/commit/377ad7b84923620927967971f8a1b7ad5a357231))
* **mctl-glovo:** hosted MCP routes behind GLOVO_C2_ENABLED (C2-2 slice 2) ([513b704](https://github.com/mctlhq/mctl-glovo/commit/513b704db7086f43c403d0c14fafe7ba7b177b7b))
* **mctl-glovo:** hosted MCP routes behind GLOVO_C2_ENABLED (C2-2 slice 2) ([66bab11](https://github.com/mctlhq/mctl-glovo/commit/66bab11ae4468ff5d7bee878f7bd4ec58ed78d05))
* **refresher:** ZITADEL machine-user token refresher sidecar ([e37d835](https://github.com/mctlhq/mctl-glovo/commit/e37d83568edb687db3442cfa48c9b758a021a5c9))
* **refresher:** ZITADEL machine-user token refresher sidecar ([da5f43f](https://github.com/mctlhq/mctl-glovo/commit/da5f43fc9defb7565a9b3c62db4021c2317938dc))


### Bug Fixes

* follow-ups from the PR 29 review (pins, migrator probe) ([155c4a0](https://github.com/mctlhq/mctl-glovo/commit/155c4a084ac164fc27fdb68a3c4c2d3fcd55ac10))
* **glovotools:** drop duplicate session-refused case after merging main ([60188a8](https://github.com/mctlhq/mctl-glovo/commit/60188a8630b174f8a139649630795e163b2c178e))
* **killswitch:** close flip races, re-kill on failed read-back, latch answer ([6d35298](https://github.com/mctlhq/mctl-glovo/commit/6d35298292dad74e108810aac417ef9ba1379796))
* **landing:** correct overclaims in page copy (P1 audit) ([9bd58e1](https://github.com/mctlhq/mctl-glovo/commit/9bd58e1f18be171c546997245cfedc9c63bd3342))
* **landing:** correct overclaims in the page copy (P1 audit findings) ([1446bb7](https://github.com/mctlhq/mctl-glovo/commit/1446bb75082cb6dc7c39522a447e8613c846665d))
* **landing:** skip link above header, self-canonical variants, CI paths, bounded read ([d5f6cc7](https://github.com/mctlhq/mctl-glovo/commit/d5f6cc78efffcce3b20e9270492b96dc712f48f3))
* **linkflow:** http only for cluster-internal consent API hosts; FIFO-safe token read ([ea0f728](https://github.com/mctlhq/mctl-glovo/commit/ea0f728d29ab6ac06563ae1e23127906eef52e76))
* **mctl-glovo:** keyed audit digest, read-deadline clear and timeout tests ([0e98128](https://github.com/mctlhq/mctl-glovo/commit/0e9812891e2a6001cda0172a9002253f693ffbf6))
* PR 29 review follow-ups (pins, migrator partial-schema probe) ([530fe0f](https://github.com/mctlhq/mctl-glovo/commit/530fe0ffa42e1f3d2854897bab92982ca662f43d))
* **token-refresher:** tighten leak/proxy tests, expiry from request start ([2ce4273](https://github.com/mctlhq/mctl-glovo/commit/2ce427339a6f56560b6d0d978aea645c02587133))
* **tokenstore:** keep refresh-failure accounting across location writes ([00b2904](https://github.com/mctlhq/mctl-glovo/commit/00b290476a6584f715a54201e88bf2dec8dce28c))


### Miscellaneous Chores

* first release is 0.1.0, not 1.0.0 ([01fcb25](https://github.com/mctlhq/mctl-glovo/commit/01fcb2524860224712738348798c0f5a75d4f3fe))
