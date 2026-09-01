#!/bin/bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Verificar env.js no S3 e invalidar CloudFront

echo "=== Verificando env.js no S3 ==="
aws s3 cp s3://aiplat-poc-bo-console-4b5ba8c3092149f1186cbc673d/env.js /tmp/bo-env.js

echo ""
echo "=== Conteúdo do env.js ==="
cat /tmp/bo-env.js

echo ""
echo "=== Invalidando CloudFront do backoffice ==="
aws cloudfront create-invalidation --distribution-id E30SI3ERXR46DQ --paths "/*"

echo ""
echo "=== Done ==="
