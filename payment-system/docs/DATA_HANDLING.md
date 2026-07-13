# Payment Data Handling

- Amounts use integer minor units where the provider supplies an unambiguous
  currency amount.
- Google purchase tokens are encrypted with AES-256-GCM. Their lookup key is a
  SHA-256 digest; rotating the encryption key requires decrypt-and-reencrypt
  migration before replacing the secret.
- Webhook payloads may contain personal/payment metadata. Successful payloads
  are removed after the configured retention period; event IDs and processing
  state remain for deduplication and audit.
- Identity IDs, customer IDs, subscription IDs, product IDs, status, amount,
  currency, and timestamps are stored. Raw card data and provider credentials
  are never stored.
- Application identity deletion must pseudonymize the identity link only when
  compatible with accounting, tax, dispute, and fraud-retention obligations.
  Payment records required by law must not be blindly cascaded.
- Disabling or removing the plugin does not delete tables. Data removal is a
  separate, explicit operational decision.
