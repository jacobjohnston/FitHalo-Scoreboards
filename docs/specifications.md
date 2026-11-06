# API and specifications

Three files define the contract between a FitHalo Scoreboard and the FitHalo apps. The server enforces
exactly these files, and the apps read the same files. This document explains the files, the sharing
model the files encode, and the layout of stored data. The [README](../README.md) explains what a
scoreboard is and defines the roles used here: the operator, the admin, and the members.

| File | Content |
|---|---|
| [`openapi.yaml`](../openapi.yaml) | The API: all operations, schemas, problem codes, conventions, limits, and the versioning rules. The source of truth for the server and the apps |
| [`spec/sharing.json`](../spec/sharing.json) | All weekly challenge data and the sharing each field requires. The server removes fields by this file, and both sides test against this file |
| [`spec/vocabularies.json`](../spec/vocabularies.json) | The known activity types, set types, and challenge tiers |

## Contents

- [The API](#the-api)
- [Sharing](#sharing)
- [The sharing file](#the-sharing-file)
- [The vocabularies file](#the-vocabularies-file)
- [Storage specification](#storage-specification)

## The API

The API is served under `/api/v1` on the scoreboard's address. All operations except `GET /board` and
`GET /board/avatar` are signed with the member's key, as the README's
[Member authentication](../README.md#member-authentication) section describes. In the API's paths and
schemas, `board` means the scoreboard.

| Operation | Purpose |
|---|---|
| `GET /board` | The scoreboard's identity and settings. Unsigned, so the FitHalo app can show the scoreboard before the member has a key on the scoreboard. Contains no member data |
| `GET /board/avatar` | The scoreboard's own avatar. Unsigned |
| `POST /members` | Join the scoreboard, signed with the key the request registers. A key the scoreboard already knows resumes its existing membership; a new key becomes a member, active on an open scoreboard or pending on one that approves members |
| `GET /members/me` | The caller's membership, including effective sharing. Works while the membership is pending, so the app can tell when the admin approves |
| `PATCH /members/me` | Change the display name or sharing. Lowering sharing deletes everything above the new level before the request is answered |
| `DELETE /members/me` | Leave the scoreboard. Deletes the membership, the avatar, all weekly challenges, and all routes |
| `PUT /members/me/avatar`, `DELETE /members/me/avatar` | Set or remove the caller's avatar. The server re-encodes the image as JPEG, removing metadata |
| `PUT /members/me/weekly-challenges/{weekStart}` | Replace the caller's whole weekly challenge for the week starting on `weekStart`. Fields above effective sharing are removed, and the response is what the server stored |
| `PUT /members/me/weekly-challenges/{weekStart}/activities/{activityID}/route`, `DELETE ...` | Set or remove one activity's route. Only when the caller shares routes, and only for an activity in the stored weekly challenge |
| `GET /member-requests` | The members waiting for approval, oldest first, with only their identifiers, names, and dates. Only for an active member the admin allows to approve requests, which `GET /members/me` reports as `canApproveMembers` |
| `POST /member-requests/{memberID}/approve`, `DELETE /member-requests/{memberID}` | Approve or reject a waiting member, with the same permission. A rejected member may ask again. Both answer `notFound` for anyone not waiting, so the permission never reaches an active member |
| `GET /weekly-challenges/{weekStart}` | The standings for one week: all active members in the scoreboard's ranking order, with a summary of each member's weekly challenge |
| `GET /members/{memberID}/weekly-challenges/{weekStart}` | One member's weekly challenge with its activities, at that member's effective sharing |
| `GET /members/{memberID}/avatar` | One member's avatar |
| `GET /members/{memberID}/weekly-challenges/{weekStart}/activities/{activityID}/route` | One activity's route |

### Conventions

- JSON everywhere except avatars, in UTF-8, with `application/json` in both directions.
- SI units: metres, seconds, kilograms, degrees Celsius. Each client shows its own units.
- Dates are `YYYY-MM-DD` and times are RFC 3339 with an offset.
- Identifiers (a member's, an activity's, the scoreboard's) are 16 bytes in unpadded base64url, 22
  characters.
- Unknown fields are ignored by both sides, so either side can add an optional field without breaking
  the other.
- Reads carry an `ETag` and honour `If-None-Match`, so a client opening a scoreboard that has not
  changed gets `304` and no body.
- Responses are gzipped when the request's `Accept-Encoding` allows gzip and a successful body is 1 KB or
  more. A gzipped response's `ETag` is weak, and `If-None-Match` accepts either form.
- Errors are RFC 9457 problem details, with a stable `code` field listed under [Problem codes](#problem-codes).

### Versions

Scoreboards and apps update separately, so all apps work with older and newer scoreboards, and all
scoreboards with older and newer apps.

- The API's version is `MAJOR.MINOR`: `openapi.yaml`'s `info.version`, and `apiVersion` on `GET /board`.
  A minor version only adds: optional fields, operations, values of open vocabularies, and problem codes.
  A major version is a new base path, such as `/api/v2`, served beside the old one.
- Features are announced on the scoreboard. Each feature a later version adds appears as a field of
  `Board`, and an absent field means the feature is off. A client reads what a scoreboard offers from
  those fields rather than from version numbers.
- An operation the server does not serve answers `unknownOperation`, never `notFound`, so a client can
  tell an older scoreboard from a missing member or weekly challenge.
- Clients name themselves in `User-Agent` as `<app>/<version> (<platform>; build <build>)`, naming
  neither the device nor its system version. A scoreboard may answer `upgradeRequired` to a client too
  old to serve; the 1.0 server never does.
- Closed values are `ScoreLevel`, `ActivityLevel`, and `weekStartDay`: both sides know all values, and a
  new one would be a new major version. Open values, such as activity types, tiers, join modes, rankings,
  member statuses, and problem codes, may grow in any minor version, and a client shows a value the
  client does not know generically rather than failing.
- Sharing grows by fields, never by levels. A kind of sharing a later version adds is a new optional field
  of `Sharing`, at its most private when absent.
- A weekly challenge field needs an entry in `spec/sharing.json`. An older server removes a field the
  server does not know, as the server removes one above the member's sharing.
- Invitation links keep their form in all versions: `fithalo://scoreboard?url=<scoreboard>&code=<invite>`
  in the QR code, and `https://<scoreboard>/join/<invite>` to share.
- Stored data is not part of the API. [Storage specification](#storage-specification) describes the
  stored layout and how a scoreboard upgrades the layout.

### Limits

| What | Limit |
|---|---|
| Display name | 1 to 40 characters, with leading and trailing space removed and each run of spaces inside collapsed to one; at least one visible character; no control, format, line-separator, private-use, or unassigned characters, apart from the joiners and tags inside an emoji |
| Members | 1,000 a scoreboard, waiting and active together; a join past the limit is refused with `memberLimitReached` |
| Custom exercise name | 1 to 100 characters, with no control, format, or line-separator characters, apart from the joiners and tags inside an emoji |
| Avatar upload | JPEG or PNG, 256 KB, at most 2048 pixels a side and 3,145,728 pixels in all; served as JPEG, at most 512 × 512 |
| Weekly challenge body | 384 KB, so a stored weekly challenge fits any backend, DynamoDB's 400 KB items included; 200 activities, 50 exercises an activity, 50 sets an exercise, 20 clusters a set |
| Route | 5,000 points, 256 KB |
| Weekly challenge storage | 1 MB for one member's weekly challenge and its routes together; an upload that would store more is refused with `payloadTooLarge` |
| Clock skew | 300 seconds either way |

### Problem codes

| Code | Status | Meaning |
|---|---|---|
| `badRequest` | 400 | The body or a parameter is malformed. `detail` says which |
| `unauthenticated` | 401 | The signature is missing, malformed, or wrong, or the member is unknown |
| `clockSkew` | 401 | `time` is more than 300 seconds from the server's clock |
| `replayed` | 401 | This exact signature was already accepted |
| `inviteInvalid` | 403 | The invite code is not the scoreboard's current one |
| `passwordIncorrect` | 403 | The scoreboard's join password is missing or wrong |
| `blocked` | 403 | The admin blocked this member from joining again |
| `memberLimitReached` | 403 | The scoreboard has 1,000 members, its limit |
| `pendingLimitReached` | 403 | As many requests are waiting for approval as the scoreboard allows |
| `memberPending` | 403 | The membership is waiting for the admin |
| `notPermitted` | 403 | The admin has not allowed this member to approve requests |
| `avatarsNotAllowed` | 403 | The scoreboard does not accept avatars |
| `routesNotShared` | 403 | The member's effective sharing has no routes |
| `upgradeRequired` | 403 | The scoreboard no longer serves this client's version. The 1.0 server never sends this code |
| `notFound` | 404 | No such member, weekly challenge, activity, route, or avatar |
| `unknownOperation` | 404 | The server does not serve this method and path, such as one a newer version added |
| `payloadTooLarge` | 413 | The body is over its limit, or the weekly challenge would store more than 1 MB |
| `unsupportedMediaType` | 415 | The body is not a type this operation accepts |
| `imageInvalid` | 422 | The avatar could not be decoded |
| `weekOutOfRange` | 422 | The week is not on the scoreboard's start day, is older than the scoreboard retains, or is after next week |
| `rateLimited` | 429 | Too many requests from this address; `Retry-After` says when to try again |
| `internal` | 500 | The server failed |
| `unavailable` | 503 | The server or its store cannot serve the scoreboard for now |

## Sharing

Each member decides, scoreboard by scoreboard, how much of their weekly challenge the scoreboard
receives. Sharing has five fields. Two of the fields, `score` and `activity`, form a ladder from most
private to least private, where each step includes the step before. The remaining three fields are
separate options.

| `score` | `activity` | The weekly challenge includes | Each activity includes |
|---|---|---|---|
| `progress` | | Percentage of goal | |
| `score` | | Score, goal, and tier as well | |
| `details` | `none` | The breakdown as well: exercise points, daily bonus, variety bonus | |
| `details` | `points` | | Type, day, base points, and bonus points |
| `details` | `summary` | | Duration and distance as well |
| `details` | `full` | | Start time and everything else the workout records, apart from the route and the strength sets |

All new memberships start at `progress`. The `activity` field applies only once `score` is `details`, and
`points` is the first step once activity sharing is turned on.

The three options are each off until the member turns the option on, and each applies only once
`activity` is anything but `none`:

| Option | Each activity includes |
|---|---|
| `routes` | Whether the activity has a route, which is then fetched separately |
| `strengthSets` | The exercises and each set's type, repetitions, duration, and distance |
| `strengthWeights` | Each set's weight as well. Requires `strengthSets` |

The options are separate from the ladder because each option reveals something different. A member may
share all sets of a strength workout without sharing a map of their neighbourhood, or share sets and
repetitions without sharing the weights.

### Effective sharing

The admin sets a maximum with the same five fields. A member's effective sharing is the lower of the
member's own choice and the scoreboard's maximum, field by field. Effective sharing is the only thing
that decides what the server stores.

### Changing sharing

A member can change their sharing at any time, and the admin can change the scoreboard's maximum at any
time. Whenever effective sharing goes down, the server deletes everything above the new level in all
stored weekly challenges before the lowering request is answered. Turning off `routes` deletes all
routes; turning off `strengthWeights` removes the weight from all sets. A weekly challenge or route that
arrives while effective sharing is going down is checked again after storage, and reduced or deleted to
the new level before the upload's response.

When effective sharing goes up, the FitHalo app uploads all weekly challenges the scoreboard retains
again, at the new level.

### History

Sharing covers all weekly challenges the scoreboard retains, not only the current one, and a member can
upload any of those weekly challenges again at any time: after editing a workout, after a late sync, or
after changing their sharing. The server does not verify a weekly challenge against anything. The server
stores what the member's app uploads.

### Removing fields above the sharing level

The server removes fields above the member's effective sharing rather than refusing the upload. A weekly
challenge that arrives with such fields is stored without those fields, and the response to the upload is
what the server stored. All weekly challenges the server serves pass through the same filter.

### Hide Start and End

Hide Start and End is an option on routes in the FitHalo app, chosen per scoreboard and applied by the
app before a route is sent: all points within 400 metres of the route's first point or last point are
removed. The server never receives the removed points.

A circle centred exactly on the start could be located by overlaying many routes and marking where the
routes leave the circle. To prevent that, FitHalo moves the circle's centre by a fixed offset of up to 100
metres, derived from the member's key for that scoreboard, as the README's
[How FitHalo derives its keys](../README.md#how-fithalo-derives-its-keys) section describes. The offset
is the same on all routes to one scoreboard and different on each scoreboard. FitHalo turns Hide Start
and End on when a member first turns on routes.

### Never sent

At no sharing level does the FitHalo app send a workout's title, the app that recorded the workout, the
member's real name, date of birth, medication settings, or anything else FitHalo knows beyond the fields
listed in `spec/sharing.json`. The display name a member enters when joining is the only name the
scoreboard receives.

### The leak test

The FitHalo app and the server run the same leak test: build a weekly challenge with all fields filled in,
encode the weekly challenge at all combinations of sharing, collect the path of each field in the output,
and check that `spec/sharing.json` allows each path at that sharing. FitHalo runs the test against what
the app sends. The server runs the test against what the server stores and what the server serves. A new
field that is not in the file fails the test until someone decides which level the field belongs to.

## The sharing file

[`spec/sharing.json`](../spec/sharing.json) lists each field a weekly challenge may contain, as a path
such as `activities[].strength.exercises[].sets[].weightKilograms`, with the sharing the field requires:
`score` and `activity` at least the level named, and `routes`, `strengthSets`, and `strengthWeights`
true where listed. An object or array is allowed when anything beneath the object or array is allowed. A
client never sends a field the client is not allowed; a server removes any such field the server receives
and never serves one. A field missing from the file is allowed at no level, so adding a field to a weekly
challenge means adding the field here. The file carries a `version` that counts changes; entries are only
added, never renamed, reused, or removed.

## The vocabularies file

[`spec/vocabularies.json`](../spec/vocabularies.json) lists the known values of the API's open
vocabularies with English names: activity types (FitHalo's workout type keys, from `outdoorRucking` to
`unknown`), set types (`warmup`, `normal`, `failure`, `myo`, `dropset`, `isometric`), and challenge tiers
(`beginner` through `deity`). Clients show their own localized names. A value missing from a list is
shown generically, never refused, so a newer client can send a type an older server or viewer does not
know. Values are never renamed or reused; a list only grows.

## Storage specification

A scoreboard stores its data as small documents in collections, on whichever backend hosts the
scoreboard: a directory, DynamoDB, or Cosmos DB. All backends store the same documents, so this
specification describes the data once for all three. The API is not tied to this layout: clients never
see storage, and storage can change without a change to the API.

This specification is a contract between releases of the server. Each later server reads data that an
earlier release stored, and the fixtures named under [Fixtures](#fixtures) prove the compatibility in
the tests.

### Storage version

The scoreboard document, stored in the `board` collection, carries `storageVersion`, the version of
the layout described here. The 1.0 server writes `1` when the server creates the scoreboard, and reads a
scoreboard document without the field as version `1`.

| Stored version | What the server does |
|---|---|
| Equal to or lower than the server's | Serves the scoreboard. A lower version is upgraded first, once a server exists that can upgrade one |
| Higher than the server's | Refuses to start. A running server answers all API requests with `unavailable` and all portal pages with an error, and logs the reason, until the server is upgraded |

The check runs on each read of the scoreboard document, so an older instance still running during a
deployment stops touching the data as soon as a newer instance changes the version.

| Kind of change | Effect on the version | Examples |
|---|---|---|
| A change an older server can still read and write safely | The version stays the same | Adding an optional field or a new collection. An older server ignores fields and collections the older server does not know |
| A change an older server must not touch | The newer server raises the version before changing any data, and an older server then refuses the scoreboard | Renaming, moving, retyping, or reinterpreting stored data, or adding data an older server would mishandle by ignoring |
| Returning to an older release after a raised version | A server never lowers the version. The operator restores the backup taken before the upgrade | |

An older server that writes a document drops the fields the older server does not know. Rolling back
across a change that added fields therefore loses the new fields in each document the older server
rewrites, though nothing else.

### Collections and documents

Collections are slash-separated paths. Identifiers are single segments of `[A-Za-z0-9_-]`, at most 64
characters. A collection's first segment never begins with `_`, which the backends reserve. Identifiers
of members and activities are 22-character unpadded base64url strings, as in the API. `<weekStart>` is
`YYYY-MM-DD`.

| Collection | Identifier | Document | Go type |
|---|---|---|---|
| `board` | `board` | The scoreboard: identity, settings, secrets, and `storageVersion` | `scoreboard.Board` |
| `board` | `avatar` | The scoreboard's own avatar, JPEG bytes | |
| `members` | Member identifier | A membership | `scoreboard.Member` |
| `blocked` | Member identifier | A blocked member's record, containing `blockedAt` | |
| `avatars` | Member identifier | A member's avatar, JPEG bytes | |
| `weeks/<weekStart>` | Member identifier | A member's weekly challenge, as stored after fields above effective sharing were removed | `scoreboard.WeeklyChallenge` |
| `routes/<memberID>/<weekStart>` | Activity identifier | An activity's route, as an encoded polyline | `scoreboard.Route` |
| `admin` | `admin` | The admin's account, passkeys included | `admin.Account` |
| `sessions` | First 16 bytes of the SHA-256 of the session token, unpadded base64url | A portal session | `admin.Session` |
| `challenges` | Random identifier | A passkey ceremony in progress | |
| `nonces` | First 16 bytes of the SHA-256 of `<memberID>:<nonce>`, unpadded base64url | An accepted nonce, containing `usedAt` | |

JSON documents use the field names in the source's `json` tags, with times in RFC 3339 and binary values
in standard base64. The fields of each document are:

| Document | Fields |
|---|---|
| The scoreboard document | `id`, `name`, `description`, `hasAvatar`, `weekStartDay`, `retentionWeeks`, `joinMode`, `maximumPending`, `passwordHash`, `avatarsAllowed`, `maximumSharing`, `ranking`, `inviteCode`, `landingMessage`, `showInvite`, `storageVersion` |
| A membership | `id`, `publicKey`, `displayName`, `status`, `canApproveMembers`, `sharing`, `hasAvatar`, `joinedAt` |
| A weekly challenge | `progress`, `score`, `goal`, `tier`, `breakdown`, `activities`, `updatedAt`, each present only when the member's effective sharing allows the field |
| A route | `polyline`, an encoded polyline of at least two points |
| The admin's account | `username`, `userHandle`, `passwordHash`, `passwordEnabled`, `passwordBreached`, `passkeys`, `usedRecoveryCodes`, `createdAt` |
| A session | `csrf`, `createdAt`, `expiresAt` |

The directory backend also keeps two files outside any collection: `.lock`, which prevents a second
server from opening the directory, and briefly `.recovery`, which `fithalo-scoreboard reset-admin`
writes for the running server.

### Document versions

Each document carries a version that counts the document's writes, starting at 1. A write names the
version the writer read, and fails with a conflict when another writer got there first; the server then
reads again and retries. A write with expected version 0 creates a document that must not exist yet,
which is how nonces are recorded without a race. This version check is what prevents two instances of the
server from overwriting each other, on Lambda and on any host that runs more than one copy.

### Packing

Each document is stored behind one format byte, so that later formats can be added without ambiguity:

| Byte | Format |
|---|---|
| `1` | The document as written |
| `2` | The document compressed with DEFLATE against [`internal/store/dictionary-1.txt`](../internal/store/dictionary-1.txt) |

The server writes whichever form is smaller. The dictionary never changes, since documents in format `2`
can only be read with the dictionary's exact bytes; a different dictionary would need a new format byte.
The bytes that packing produces may differ between Go releases, but all releases read both formats. A
stored document may expand to at most 8 MB.

### Backends

| Backend | Layout | Concurrency |
|---|---|---|
| Directory | Each collection is a folder and each document a file named `<identifier>.json`, containing `{"version": <version>, "base64": "<packed document>"}`, written atomically | One process owns the directory: the server locks the directory on start, and a mutex serialises all access |
| DynamoDB | One table. Each item has the collection (`c`, partition key), the identifier (`i`, sort key), the packed document (`d`), and the version (`v`). The `_collections` partition records all collections, so listing collections needs no scan | Conditional writes on `v`. Nonces live in the table, so all Lambda copies see them |
| Cosmos DB | One container, partitioned by collection (`/c`). Each item has the identifier (`id`), the packed document in base64 (`d`), and the version (`v`). The `_collections` partition records all collections | ETags guard writes. Strong consistency |

The DynamoDB and Cosmos DB backends call each service's HTTP API directly, signed with AWS Signature
Version 4 and with an HMAC of the account key respectively, so neither backend needs a vendor library.

### Retention and sweeps

An hourly sweep deletes all weekly challenges, and all routes in them, older than the scoreboard's
retention setting; ended portal sessions and expired passkey ceremonies; and, on backends that store
nonces, nonces older than 600 seconds. On Lambda, an EventBridge schedule runs the sweep.

### Fixtures

The tests keep documents exactly as the 1.0 server stores them, and check that the current server decodes
each field of each one, knows no field less, and serves the documents from where 1.0 stores them:

| Fixtures | Content |
|---|---|
| `internal/scoreboard/testdata/storage-1` | The scoreboard document, a member, a blocked record, a weekly challenge with all fields, and a route |
| `internal/admin/testdata/storage-1` | The admin's account with a passkey, a session, and a challenge |
| `internal/nonces/testdata/storage-1` | A nonce record |
| `internal/store/testdata/storage-1` | A directory store with documents in both packed formats, and the bytes each one contains |

The fixtures never change. A later layout adds fixtures of its own beside the `storage-1` fixtures, such
as `storage-2`, and the fixtures of all earlier layouts keep passing through the upgrade.
