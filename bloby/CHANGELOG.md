# Changelog

## [1.3.0](https://github.com/woodleighschool/goodies/compare/bloby/v1.2.0...bloby/v1.3.0) (2026-10-09)


### Features

* **go:** update module github.com/aws/aws-sdk-go-v2/service/s3 (v1.113.4 → v1.114.0) ([#62](https://github.com/woodleighschool/goodies/issues/62)) ([5337969](https://github.com/woodleighschool/goodies/commit/5337969742bc827a58286aa90f4418f373d9b1c9))


### Bug Fixes

* **bloby:** retain objects owned by active finalization ([493c6b3](https://github.com/woodleighschool/goodies/commit/493c6b3a808165b5ab7cb4e841c897c7df016c66))
* **go:** update aws-sdk-go-v2 monorepo ([#53](https://github.com/woodleighschool/goodies/issues/53)) ([033259d](https://github.com/woodleighschool/goodies/commit/033259d19bf98ca86f25bbee9cbe2743056565e5))
* **go:** update aws-sdk-go-v2 monorepo ([#76](https://github.com/woodleighschool/goodies/issues/76)) ([ecd82ec](https://github.com/woodleighschool/goodies/commit/ecd82ec4d1aea80c3073c2d2ed37feae0dc48f73))
* **go:** update aws-sdk-go-v2 monorepo ([#84](https://github.com/woodleighschool/goodies/issues/84)) ([f5590a1](https://github.com/woodleighschool/goodies/commit/f5590a1a7079f39ce53aa95a1108d7bf16e132ef))
* **go:** update module github.com/aws/aws-sdk-go-v2/service/s3 (v1.113.1 → v1.113.2) ([#48](https://github.com/woodleighschool/goodies/issues/48)) ([596f8bc](https://github.com/woodleighschool/goodies/commit/596f8bcf33b64531e3526009b64e4e000640eacf))
* **go:** update module github.com/aws/smithy-go (v1.28.2 → v1.28.3) ([#75](https://github.com/woodleighschool/goodies/issues/75)) ([cfda656](https://github.com/woodleighschool/goodies/commit/cfda656fecd15ac59f4f168f37ada503d62e0c8f))
* **go:** update module github.com/aws/smithy-go (v1.28.3 → v1.28.4) ([#77](https://github.com/woodleighschool/goodies/issues/77)) ([ee2d0f3](https://github.com/woodleighschool/goodies/commit/ee2d0f3385e6f5f173bdbbb32aa8d674bfeb3d91))

## [1.2.0](https://github.com/woodleighschool/goodies/compare/bloby/v1.1.0...bloby/v1.2.0) (2026-09-19)


### Features

* **bloby:** sweep unreferenced objects under referenced prefixes ([833b2cc](https://github.com/woodleighschool/goodies/commit/833b2ccb3dd04f99a8dffe39a03439d5b6711ce0))


### Bug Fixes

* **go:** update aws-sdk-go-v2 monorepo ([#34](https://github.com/woodleighschool/goodies/issues/34)) ([f11fdc1](https://github.com/woodleighschool/goodies/commit/f11fdc123bc8add9862d0dddb1a1847224f4e4aa))
* **go:** update module github.com/aws/smithy-go (v1.28.1 → v1.28.2) ([#41](https://github.com/woodleighschool/goodies/issues/41)) ([2628340](https://github.com/woodleighschool/goodies/commit/26283407da3f01c0e8d2ccb2db8373dfec2eaaa4))

## [1.1.0](https://github.com/woodleighschool/goodies/compare/bloby/v1.0.0...bloby/v1.1.0) (2026-09-12)


### Features

* **go:** update aws-sdk-go-v2 monorepo ([#27](https://github.com/woodleighschool/goodies/issues/27)) ([01a880e](https://github.com/woodleighschool/goodies/commit/01a880ea64649032aaeef852a0c5f5cc4c44ceaa))
* **go:** update module github.com/jackc/pgx/v5 (v5.10.0 → v5.11.0) ([#28](https://github.com/woodleighschool/goodies/issues/28)) ([ba672b8](https://github.com/woodleighschool/goodies/commit/ba672b8b901e64083d88b5796f303b46f5350d98))
* **go:** update module github.com/pressly/goose/v3 (v3.27.3 → v3.28.0) ([#29](https://github.com/woodleighschool/goodies/issues/29)) ([8551eb8](https://github.com/woodleighschool/goodies/commit/8551eb8cee75dc65e94bb24702fe0e0eea38ef81))


### Miscellaneous Chores

* move to mise monorepo ([7450e2e](https://github.com/woodleighschool/goodies/commit/7450e2e11907ea1f1341663f01ecfeedc9bf8888))

## [1.0.0](https://github.com/woodleighschool/goodies/compare/bloby/v0.2.0...bloby/v1.0.0) (2026-09-05)


### ⚠ BREAKING CHANGES

* simplify shared service boundaries ([#10](https://github.com/woodleighschool/goodies/issues/10))

### Features

* simplify shared service boundaries ([#10](https://github.com/woodleighschool/goodies/issues/10)) ([b9f0c2a](https://github.com/woodleighschool/goodies/commit/b9f0c2af22d5b0b0a92bb9758372031baba3a6e9))

## [0.2.0](https://github.com/woodleighschool/goodies/compare/bloby/v0.1.1...bloby/v0.2.0) (2026-09-03)


### Features

* complete auth and blob lifecycle ownership ([e0024a6](https://github.com/woodleighschool/goodies/commit/e0024a656a81c3be640ec27920ea032e9b7f0530))

## [0.1.1](https://github.com/woodleighschool/goodies/compare/bloby/v0.1.0...bloby/v0.1.1) (2026-09-02)


### Bug Fixes

* **bloby:** narrow object cleanup dependency ([d0cef82](https://github.com/woodleighschool/goodies/commit/d0cef827df9a8cbdcaaa6f41c9df35087f0524d7))

## 0.1.0 (2026-09-02)


### Features

* add Bloby storage modules ([073da37](https://github.com/woodleighschool/goodies/commit/073da37c5eb80bc15aa43321fa9806691b953696))
