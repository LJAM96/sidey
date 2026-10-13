# Sidey Auto Resign Implementation Plan

## Objective

Rework the current auto resign flow so that every refresh operation has an explicit and reliable relationship between:

• the deployment being refreshed

• the source IPA

• the signed artifact currently installed

• the Apple account used to sign it

• the certificate and provisioning profile

• the install operation that completed the refresh

The refresh system should stop reconstructing this information from whichever install job happened most recently on a device.

The final intended flow should be:

```text
Deployment becomes due
        ↓
Create refresh job
        ↓
Resolve current signed artifact
        ↓
Resolve original source IPA
        ↓
Resolve original Apple account
        ↓
Create child sign job
        ↓
Signing worker produces signed artifact
        ↓
Create child install job
        ↓
Device service performs install only
        ↓
Verify profile expiry
        ↓
Complete refresh
        ↓
Update deployment and installation record
```

The installation stage must never authenticate with Apple or sign an IPA.

---

# Data Model Changes

## Extend installation_records

Add a direct relationship between an installation and the signed artifact that was actually installed.

Add:

```sql
ALTER TABLE installation_records
ADD COLUMN signed_artifact_id uuid
REFERENCES signed_artifacts(id);

ALTER TABLE installation_records
ADD COLUMN install_job_id uuid
REFERENCES jobs(id);

CREATE INDEX idx_installation_signed_artifact
ON installation_records(signed_artifact_id);
```

The important field is:

```text
signed_artifact_id
```

This becomes the authoritative answer to:

```text
Which signed IPA is currently installed for this deployment?
```

The scheduler should no longer infer this from job history.

---

## Extend jobs for workflow relationships

Add explicit parent and purpose information.

Suggested fields:

```sql
ALTER TABLE jobs
ADD COLUMN parent_job_id uuid
REFERENCES jobs(id);

ALTER TABLE jobs
ADD COLUMN purpose text;

CREATE INDEX idx_jobs_parent
ON jobs(parent_job_id);
```

Typical values for purpose:

```text
deploy
refresh
manual_refresh
update
```

This allows Sidey to understand that:

```text
refresh job A
    sign job B
        install job C
```

are one workflow.

It will also make the dashboard much easier to improve later.

---

# Fix Install Only Mode First

This should be the first code change because it directly affects the current hanging behaviour.

## Current problem

The wireless installer receives:

```text
SKIP_SIGN=1
```

but the Rust binary performs Apple authentication before checking that value.

Install only therefore still performs:

```text
Apple login
2FA
DeveloperSession creation
```

before installing the already signed IPA.

That defeats the purpose of the split signing architecture.

---

## Required Rust restructuring

In:

```text
third_party/isideload/examples/wireless/src/main.rs
```

the application should determine its mode immediately after parsing the arguments and environment.

Conceptually:

```rust
let skip_sign = env::var("SKIP_SIGN").as_deref() == Ok("1");
```

Then initialise:

```text
application path
device UDID
RSD address
RSD port
```

Connect to RSD.

If:

```text
skip_sign == true
```

perform:

```text
extract IPA
read embedded.mobileprovision
connect to RSD
install application
report expiry
exit
```

Do not construct:

```text
RemoteV3AnisetteProvider
AppleAccount
DeveloperSession
Sideloader
```

at all.

Only the normal sign and install code path should initialise those objects.

The two flows should look like:

```text
INSTALL ONLY

parse args
↓
open signed IPA
↓
connect RSD
↓
install
↓
extract profile expiry
↓
success
```

and:

```text
SIGN AND INSTALL

parse args
↓
Apple authentication
↓
developer session
↓
sign
↓
connect RSD
↓
install
↓
success
```

Eventually the second path may become unnecessary if all signing is centralised in the signing worker.

---

# Remove Apple Credentials From Install Only Wrapper

In:

```text
scripts/wireless-install.sh
```

the script currently still sources Apple credentials regardless of whether signing is required.

Change the wrapper so:

```text
SKIP_SIGN=1
```

uses a completely credential free invocation.

Conceptually:

```bash
if [ "${SKIP_SIGN:-}" = "1" ]; then
    env \
        RSD_ADDR="$RSD_ADDR" \
        RSD_PORT="$RSD_PORT" \
        DEVICE_UDID="$DEVICE_UDID" \
        SKIP_SIGN=1 \
        "$BIN" install-only "$IPA_PATH"
else
    source /usr/local/sbin/sidey-creds.sh

    env \
        SIDEY_APPLE_MAIN_PASSWORD="$SIDEY_APPLE_MAIN_PASSWORD" \
        ...
        "$BIN" "$SIDEY_APPLE_ID" "$IPA_PATH"
fi
```

The preferred longer term design would be a dedicated install command rather than overloading the existing binary.

For example:

```text
wireless install signed.ipa
```

and:

```text
wireless sign-install original.ipa
```

This makes accidental Apple authentication much harder to reintroduce.

---

# Make Refresh Apple Account Aware

## Current problem

A normal deployment has an explicit:

```text
apple_id
```

but the newly created refresh sign job does not.

The signing worker therefore falls back to whichever account it happens to retrieve from its account map.

That must be removed.

---

## Account source of truth

When a deployment is refreshed, Sidey should use the Apple account associated with the currently installed signed artifact.

The relationship becomes:

```text
deployment
↓
installation_records.signed_artifact_id
↓
signed_artifacts.account_id
↓
apple_accounts.label
```

The scheduler should therefore resolve:

```text
source_artifact_id
signed_artifact_id
account_id
apple_id
```

when creating the refresh.

Suggested refresh parameters:

```json
{
  "deployment_id": "...",
  "source_artifact_id": "...",
  "previous_signed_artifact_id": "...",
  "apple_account_id": "...",
  "apple_id": "...",
  "udid": "...",
  "device_name": "...",
  "expiry_at": "..."
}
```

The refresh endpoint should reject the request if an Apple account cannot be resolved.

It should never silently select one.

---

# Remove Arbitrary Account Selection From Signing Worker

The behaviour inside:

```go
getCredentials()
```

should also be tightened.

If multiple accounts exist and no Apple ID was supplied, do not select an arbitrary map entry.

Instead return an error such as:

```text
sign job does not specify apple_id and multiple accounts are configured
```

A fallback may be acceptable when exactly one account exists.

Suggested behaviour:

```text
requested account exists
    use it

requested account missing
    fail

no requested account and exactly one configured
    use it

no requested account and multiple configured
    fail
```

This prevents future bugs outside auto refresh as well.

---

# Replace Device Level Artifact Resolution

## Current problem

The scheduler currently searches for:

```text
latest completed install job on this device
```

That is not sufficient when multiple applications exist on the same iPhone.

---

## New scheduler resolution

The scheduler should no longer inspect install jobs to determine what application to refresh.

Instead:

```sql
SELECT
    dep.id,
    dep.device_id,
    ir.signed_artifact_id,
    sa.source_artifact_id,
    sa.account_id,
    acc.label,
    ir.provisioning_expiry_at
FROM deployments dep
JOIN installation_records ir
    ON ir.deployment_id = dep.id
JOIN signed_artifacts sa
    ON sa.id = ir.signed_artifact_id
JOIN apple_accounts acc
    ON acc.id = sa.account_id
...
```

The refresh job can then carry exact values.

No heuristic is required.

---

# Update Installation Record Creation

Whenever an install job completes successfully, Sidey should record exactly what was installed.

The device service already knows the signed artifact ID.

When the install succeeds, update or insert:

```text
deployment_id
signed_artifact_id
install_job_id
installed_version
installed_build
installed_bundle_identifier
provisioning_expiry_at
verified_at
installation_method
```

This information should be committed transactionally where possible.

The resulting record becomes the authoritative state for that deployment.

---

# Eliminate Duplicate Refresh Installs

## Current problem

A sign upload automatically creates an install job.

At the same time, the refresh orchestrator waits for the sign job, downloads the signed IPA, and performs an install itself.

One refresh can therefore generate two installs.

---

## Recommended approach

Make refresh a parent workflow.

The refresh orchestrator should never perform the install directly.

Instead:

```text
refresh job
↓
create sign child
↓
wait for sign child
↓
sign upload creates install child
↓
wait for install child
↓
complete refresh
```

This provides one consistent installation path for:

```text
initial deployment
manual deployment
automatic refresh
manual refresh
application update
```

The device service then only knows how to perform install jobs.

The signing worker only knows how to perform signing jobs.

The refresh workflow coordinates them.

---

# Change Signed Artifact Upload Behaviour

Currently:

```text
signed artifact uploaded
↓
create install job
```

Keep this behaviour, but make the relationship explicit.

When the sign job has:

```text
parent_job_id = refresh job
```

create the install job with:

```text
parent_job_id = refresh job
purpose = refresh
```

and potentially:

```text
source_sign_job_id
signed_artifact_id
deployment_id
```

Example parameters:

```json
{
  "deployment_id": "...",
  "signed_artifact_id": "...",
  "source_sign_job_id": "...",
  "device_udid": "...",
  "platform": "ios",
  "bundle_id": "..."
}
```

The refresh coordinator can then discover the child install deterministically.

---

# Refresh Coordinator

The existing `_run_refresh_ios()` logic is doing too much work.

Its responsibility should become workflow coordination only.

Conceptually:

```python
def run_refresh(job):

    mark_refresh_in_progress()

    sign_job = ensure_sign_job(job)

    wait_for(sign_job)

    if sign_job.failed:
        fail_refresh()

    install_job = find_or_create_install_job(sign_job)

    wait_for(install_job)

    if install_job.failed:
        fail_refresh()

    result = install_job.result

    complete_refresh(
        profile_expiry_at=result["profile_expiry_at"],
        signed_artifact_id=install_job.parameters["signed_artifact_id"]
    )
```

The coordinator does not:

```text
download IPA
run installer
authenticate with Apple
open RSD
```

Those belong to specialised workers.

---

# Refresh Completion

When the refresh parent completes successfully:

Update:

```text
deployments.last_refresh_at
deployments.last_refresh_result
deployments.next_refresh_due_at
```

Update the installation record:

```text
signed_artifact_id = new signed artifact
provisioning_expiry_at = new expiry
verified_at = now()
install_job_id = child install job
```

The old signed artifact can remain in the database for history and audit purposes.

---

# Refresh Failure Handling

The refresh parent should propagate meaningful errors from its child jobs.

For example:

```text
sign/auth
sign/certificate
sign/provisioning
sign/app_id_quota
install/rsd_unavailable
install/device_locked
install/device_offline
install/profile_invalid
install/timeout
```

Do not convert every orchestration failure into:

```text
sign_failed
```

as the current `_run_refresh_ios()` effectively does.

Store:

```text
failed_stage
child_job_id
error_category
error_details
```

either in the refresh job result or dedicated columns.

Example:

```json
{
  "failed_stage": "install",
  "child_job_id": "...",
  "error_category": "rsd_unavailable",
  "error_details": "RSD endpoint unavailable after tunnel restart"
}
```

This will make the dashboard much more useful.

---

# Retry Behaviour

Retries should occur at the appropriate layer.

A failed install should not necessarily cause another signing operation.

For example:

```text
sign completed successfully
↓
install failed because device was offline
```

On retry, Sidey should reuse the signed artifact until its provisioning profile becomes unsuitable.

The refresh parent should therefore remember:

```text
signed_artifact_id
```

after the sign step succeeds.

Then an install retry becomes:

```text
existing signed artifact
↓
new install attempt
```

rather than:

```text
sign again
↓
register App ID again
↓
create another signed artifact
↓
install
```

This will also reduce Apple Developer API usage.

---

# Idempotency

Use deterministic keys for child jobs.

Example sign child:

```text
refresh-sign:<refresh_job_id>
```

Example install child:

```text
refresh-install:<refresh_job_id>:<signed_artifact_id>
```

Repeated execution of the parent should therefore resume the existing workflow rather than create another sign or install operation.

The current refresh sign idempotency concept is good and should be retained.

---

# Scheduler Changes

The scheduler should create a refresh only when the deployment has enough authoritative state to refresh safely.

A deployment should be considered refreshable only if:

```text
installation record exists
signed_artifact_id exists
source artifact exists
Apple account exists
device exists
device service exists
profile expiry exists
```

If any are missing, mark:

```text
requires_attention
```

rather than attempting to guess.

---

# Manual Refresh

The dashboard's Refresh Now action should use the same workflow as automatic refresh.

It should only create a parent refresh job.

Do not send the artifact ID from the browser.

The server already knows the deployment ID and should resolve all authoritative state itself.

Recommended request:

```json
{
  "deployment_id": "...",
  "reason": "manual"
}
```

This removes another opportunity for stale UI state to select the wrong IPA.

---

# Dashboard Changes

The refresh section should expose the workflow rather than only the parent state.

Suggested states:

```text
Scheduled
Preparing signature
Signing
Waiting for device
Installing
Verifying
Complete
Needs attention
```

When a refresh fails, show:

```text
Stage
Attempt
Error
Last successful signing
Profile expiry
Apple account
```

Example:

```text
Auto refresh: Needs attention

Stage: Installing
Reason: iPhone unavailable
Apple account: luke@example.com
Signed successfully: 14:32
Next retry: 14:37
Profile expires: 7 September
```

This will make troubleshooting much easier than a generic refresh failure.

---

# Migration Strategy

Existing installation records will not have:

```text
signed_artifact_id
```

These can be backfilled where the relationship is unambiguous.

Use:

```text
installation record
device
installed bundle identifier
signed artifacts for device
signed bundle identifier
profile expiry proximity
```

Only populate records where there is one clear match.

Do not guess ambiguous mappings.

Anything that cannot be safely resolved should appear in the dashboard as:

```text
Legacy deployment needs reassociation
```

A manual reassociation action can be provided if necessary.

---

# Testing Plan

## Install only test

Start with:

```text
SKIP_SIGN=1
```

and deliberately configure invalid Apple credentials.

The install must still succeed.

This proves Apple authentication is completely absent from the install only path.

---

## Two application test

One iPhone with:

```text
SideStore
LiveContainer
```

Both deployments should independently refresh their own source IPA.

Refresh SideStore.

Confirm LiveContainer is untouched.

Refresh LiveContainer.

Confirm SideStore is untouched.

---

## Two account test

Configure:

```text
Account A
Account B
```

Install SideStore with A.

Install LiveContainer with B.

Refresh each application.

Confirm the resulting signed artifact retains the corresponding account.

---

## Authentication failure test

Expire the credentials for Account B.

Refresh the Account A application.

It must still succeed.

Refresh the Account B application.

It must fail clearly with:

```text
auth
```

and must not silently fall back to Account A.

---

## RSD failure test

Complete signing successfully.

Disable the RSD tunnel.

Allow install to fail.

Restore the tunnel.

Retry refresh.

Confirm the existing signed artifact is reused rather than another sign job being created.

---

## Restart recovery test

Restart the control plane after signing completes but before installation.

The refresh should resume from the existing child jobs.

Repeat with a restart during installation.

No duplicate signing should occur.

---

## Idempotency test

Submit the same refresh parent several times.

Confirm exactly one:

```text
sign child
```

and one successful:

```text
install child
```

are produced.

---

# Recommended Implementation Order

## Immediate stability work

Fix the Rust `SKIP_SIGN` branch.

Remove credentials from the install only wrapper.

Add strict Apple account selection.

These changes directly address the current hanging behaviour.

## Persistence changes

Add:

```text
installation_records.signed_artifact_id
installation_records.install_job_id
jobs.parent_job_id
jobs.purpose
```

Update normal deployment installation recording.

## Scheduler changes

Replace device level latest install lookup.

Resolve source artifact and account through the installation record.

Remove artifact selection from the dashboard client.

## Workflow changes

Convert refresh into a parent workflow.

Make sign and install explicit child jobs.

Remove manual installation from `_run_refresh_ios()`.

Ensure only one install is created.

## Retry improvements

Reuse a completed signed artifact when only installation failed.

Improve stage specific error propagation.

## Dashboard improvements

Expose child workflow state.

Display account, stage, retry state and useful failure information.

## Integration testing

Add multi app and multi account scenarios.

These should become mandatory regression tests because they are exactly the cases that exposed the current architecture weaknesses.

---

# Intended End State

After the changes, Sidey should have a very predictable auto resign lifecycle.

```text
Deployment
    ↓
Current installation record
    ↓
Current signed artifact
    ↓
Source IPA + Apple account
    ↓
Refresh parent
    ↓
Sign child
    ↓
New signed artifact
    ↓
Install child
    ↓
New installation record state
    ↓
Next refresh scheduled
```

Nothing should rely on the newest install job for an entire device.

Nothing should choose an Apple account implicitly.

Nothing in the installation path should require Apple credentials.

Nothing should be signed twice during one refresh.

A temporary device connectivity problem should not cause unnecessary re signing.

This also gives Sidey a much stronger foundation for future application updates, certificate rotation, multiple Apple accounts, multiple devices and more detailed refresh monitoring.

