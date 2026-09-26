#!/bin/bash
# LocalStack keeps no state across restarts, so the documents bucket must be
# recreated every time the container comes up — without it every upload
# fails with "Could not store the uploaded file".
set -euo pipefail
bucket="${S3_BUCKET_DOCUMENTS:-founderstack-documents}"
if awslocal s3api head-bucket --bucket "$bucket" 2>/dev/null; then
  echo "bucket $bucket already exists"
else
  awslocal s3 mb "s3://$bucket"
fi
