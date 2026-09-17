TypeSafe evaluation and rollout
==============================

Keep the server in `off` or `shadow` while choosing thresholds. The evaluation command sends selected local email content to TypeSafe and writes predictions; it never delivers messages or changes stored mail. It uses the same extractor and questions as inbound SMTP.

Create a JSONL manifest with one file and label per line. Relative paths resolve against the manifest directory:

```jsonl
{"path":"mail/receipt.eml","label":"ham"}
{"path":"mail/unsolicited-offer.eml","label":"spam"}
{"path":"mail/fake-login.eml","label":"phishing"}
```

Use `ham` for wanted mail. Label an active phishing attempt `phishing`; label other unwanted mail `spam`. The summary treats spam and phishing together as unwanted mail, while the prediction rows retain both probabilities. Use a corpus representative of the messages that pass existing checks, because the new stage skips messages already rejected or classified as Junk for every recipient.

An optional `auth_results` object can contain server-computed SPF, DKIM and DMARC results exported with the corpus. Do not populate it from untrusted `Authentication-Results` headers in the email. The command neither trusts those headers nor performs historical DNS verification.

With `HAITATSU_TYPESAFE_API_KEY` set in the environment, run:

```sh
go run ./cmd/typesafe-eval -corpus labels.jsonl > predictions.jsonl 2> summary.json
```

The key has no command-line flag and does not appear in help output. The command validates the complete manifest before making requests. It paces requests at 60 per minute by default. `-max-requests-per-minute` changes that budget; coordinate it with other processes using the account.

Use `-model`, `-timeout-ms`, `-max-text-bytes`, `-spam-threshold`, and `-phishing-threshold` to test settings. Defaults match the server. The model alias `jev-latest` can change, so use an available fixed model version when comparing experiments and retain the returned model identifiers.

The command does not load mailbox overrides. To evaluate an inbox's TypeSafe thresholds, pass its effective values with `-spam-threshold` and `-phishing-threshold`, including server defaults for any keys the inbox inherits.

Predictions contain the source path and label, requested and returned model, question/extractor versions, probabilities, thresholds, proposed Junk action, extraction limits, latency and token usage. Bodies and keys do not appear in output. Read failures, unusable content and provider errors produce an unscored row with null probabilities and action.

On successful completion, stderr contains one JSON summary. Precision, recall and the legitimate-mail false-positive rate use successful evaluations only; failures never count as negative predictions. The summary also reports unscored cases, fallback rate, request latency percentiles and token totals. A metric with no eligible denominator is null. Always review coverage alongside accuracy, since failures and truncation can hide difficult cases. Summary rates describe the combined Junk decision, not separate phishing accuracy.

Include receipts, account recovery, subscribed newsletters, personal correspondence, cold outreach, multilingual mail and deceptive messages. Keep each thread or campaign in one dataset split. Choose thresholds on tuning data, then evaluate held-out mail without changing those thresholds. The default `0.98` values do not guarantee a false-positive rate; a target such as fewer than 0.1% false positives needs enough legitimate samples and a statistical uncertainty interval.

The command does not estimate prices. Use its token totals with current account rates. Confirm provider content-retention and processing terms before running private mail through it. Email bodies can contain personal information even when URL queries, recipients and internal metadata are omitted.

After evaluation, set the server's mode to `shadow` and inspect `auth_results.typesafe` plus `typesafe_*` metrics under representative traffic. Use the observed fallback rate and latency to tune limits. Enable `junk` when the observed error rates are acceptable. Setting `off` stops subsequent assessments; users can restore false positives through existing IMAP or API moves, which do not retrain TypeSafe.
