# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0

# Frontend domain — reusable MODULE (backend/provider live in the env wrapper).
# Serves the console (console.html) via private S3 + CloudFront/OAC. No public landing
# — the sales page was removed from the project; the console is the only surface and
# can, optionally, sit behind its own authentication proxy at the edge.
terraform {
  required_version = ">= 1.9.0, < 2.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 6.0.0, < 7.0.0" }
  }
}

# site_path is passed by the env (where path.module = the env folder), preserving the
# exact source/filemd5 string and avoiding a diff when migrating from envs/poc to tf/.
variable "site_path" {
  type = string
}

variable "region" {
  type    = string
  default = "us-west-2"
}
variable "project" {
  type    = string
  default = "aiplat"
  validation {
    condition     = can(regex("^[a-z0-9]+$", var.project))
    error_message = "project must match ^[a-z0-9]+$ (no hyphens/uppercase)."
  }
}
variable "environment" {
  type    = string
  default = "poc"
  validation {
    condition     = can(regex("^[a-z0-9]+$", var.environment))
    error_message = "environment must match ^[a-z0-9]+$ (no hyphens/uppercase)."
  }
}

# --- Input contracts (Req 4.5): endpoints come from the Environment Contract (SSM);
# they are all NON-derivable URLs/IDs. Each has an optional override (default null). ---
variable "admin_api_endpoint" {
  type    = string
  default = null
}
variable "cognito_client_id" {
  type    = string
  default = null
}
variable "gateway_endpoint" {
  type    = string
  default = null
}
variable "keyadmin_endpoint" {
  type    = string
  default = null
}
variable "usage_api_endpoint" {
  type    = string
  default = null
}

# --- Custom domain (optional, but the only way to raise the TLS floor) ---------
# Leaving both null keeps the CloudFront default certificate, which AWS pins to a
# minimum of TLSv1 — acceptable for a throwaway PoC, not for production. Supply
# both to serve the console over your own domain with TLSv1.2_2021 enforced.
variable "domain_name" {
  description = "Custom domain for the console (e.g. console.example.com). null = use the CloudFront default domain."
  type        = string
  default     = null
}
variable "acm_certificate_arn" {
  description = "ACM certificate ARN for domain_name. MUST be issued in us-east-1 (CloudFront requirement). null = CloudFront default certificate (TLS floor stays at TLSv1)."
  type        = string
  default     = null
  validation {
    condition     = var.acm_certificate_arn == null || can(regex("^arn:aws:acm:us-east-1:", var.acm_certificate_arn))
    error_message = "acm_certificate_arn must be an ACM certificate in us-east-1 (CloudFront only accepts certificates from that region)."
  }
}

variable "help_api_endpoint" {
  type    = string
  default = null
}
variable "audit_api_endpoint" {
  type    = string
  default = null
}

locals {
  name     = "${var.project}-${var.environment}-fe"
  site     = var.site_path
  ssm_root = "/${var.project}/${var.environment}"

  # Resolution override > Contract (SSM). nonsensitive(): public URLs/IDs.
  # trimsuffix("/"): stage URLs come with a trailing slash; env.js was always
  # written without one (the .tftpl strips it at runtime), so we trim it to a no-op.
  admin_api_endpoint = trimsuffix(coalesce(var.admin_api_endpoint, try(nonsensitive(data.aws_ssm_parameter.admin_api_url[0].value), null)), "/")
  cognito_client_id  = coalesce(var.cognito_client_id, try(nonsensitive(data.aws_ssm_parameter.cognito_client_id[0].value), null))
  gateway_endpoint   = trimsuffix(coalesce(var.gateway_endpoint, try(nonsensitive(data.aws_ssm_parameter.gateway_url[0].value), null)), "/")
  keyadmin_endpoint  = trimsuffix(coalesce(var.keyadmin_endpoint, try(nonsensitive(data.aws_ssm_parameter.keyadmin_url[0].value), null)), "/")
  usage_api_endpoint = trimsuffix(coalesce(var.usage_api_endpoint, try(nonsensitive(data.aws_ssm_parameter.usage_api_url[0].value), null)), "/")
  help_api_endpoint  = trimsuffix(coalesce(var.help_api_endpoint, try(nonsensitive(data.aws_ssm_parameter.help_api_url[0].value), null)), "/")
  # Empty when the audit domain does not exist yet — the console hides the audit
  # sub-tabs instead of showing a tab that never loads.
  audit_api_endpoint = trimsuffix(coalesce(var.audit_api_endpoint, try(nonsensitive(data.aws_ssm_parameter.audit_api_url[0].value), ""), ""), "/")

  # Social login (Governance contract). Absent/"none" = no buttons in the console.
  cognito_domain   = trimsuffix(try(nonsensitive(data.aws_ssm_parameter.cognito_domain[0].value), ""), "/")
  social_providers = try(nonsensitive(data.aws_ssm_parameter.social_providers[0].value), "none")
}

data "aws_ssm_parameter" "admin_api_url" {
  count = var.admin_api_endpoint == null ? 1 : 0
  name  = "${local.ssm_root}/governance/admin_api_url"
}
data "aws_ssm_parameter" "cognito_client_id" {
  count = var.cognito_client_id == null ? 1 : 0
  name  = "${local.ssm_root}/governance/cognito_client_id"
}
data "aws_ssm_parameter" "gateway_url" {
  count = var.gateway_endpoint == null ? 1 : 0
  name  = "${local.ssm_root}/core/gateway_url"
}
data "aws_ssm_parameter" "keyadmin_url" {
  count = var.keyadmin_endpoint == null ? 1 : 0
  name  = "${local.ssm_root}/core/keyadmin_url"
}
data "aws_ssm_parameter" "usage_api_url" {
  count = var.usage_api_endpoint == null ? 1 : 0
  name  = "${local.ssm_root}/observability/usage_api_url"
}
data "aws_ssm_parameter" "audit_api_url" {
  count = var.audit_api_endpoint == null ? 1 : 0
  name  = "${local.ssm_root}/audit/audit_api_url"
}
data "aws_ssm_parameter" "help_api_url" {
  count = var.help_api_endpoint == null ? 1 : 0
  name  = "${local.ssm_root}/help/help_api_url"
}
data "aws_ssm_parameter" "cognito_domain" {
  count = 1
  name  = "${local.ssm_root}/governance/cognito_domain"
}
data "aws_ssm_parameter" "social_providers" {
  count = 1
  name  = "${local.ssm_root}/governance/social_providers"
}

# Platform account — goes as a PARAMETER in the BYO CloudFormation template
# (the client's role trusts this account). Not a secret.
data "aws_caller_identity" "current" {}

# --- Static site (private S3 + CloudFront/OAC) ---
resource "aws_s3_bucket" "site" {
  bucket_prefix = "${local.name}-site-"
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "site" {
  bucket                  = aws_s3_bucket.site.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_cloudfront_origin_access_control" "site" {
  name                              = "${local.name}-site-oac"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

# --- Security response headers (CSP et al.) ------------------------------------
# Without a Content-Security-Policy the console has no origin restriction on what
# it may load or talk to. The policy below is deliberately explicit:
#
#   script-src 'self' + jsDelivr — Tailwind is self-hosted (same origin) and
#     Chart.js is pinned with Subresource Integrity; no other origin may run JS.
#     'unsafe-inline' is required because console.html is a single file with
#     inline <script> blocks (no build step, by design). It does NOT need
#     'unsafe-eval': the vendored Tailwind build contains no eval/new Function
#     (verified against the downloaded artifact).
#   style-src 'unsafe-inline' — the Tailwind runtime injects <style> elements.
#   connect-src — the console only ever calls this deployment's APIs
#     (API Gateway) and Cognito, both scoped to the deployment region.
#   frame-ancestors 'none' — the console must never be embedded (clickjacking).
resource "aws_cloudfront_response_headers_policy" "site" {
  name    = "${local.name}-security-headers"
  comment = "CSP and hardening headers for the ${local.name} console"

  security_headers_config {
    content_security_policy {
      override = true
      content_security_policy = join("; ", [
        "default-src 'none'",
        "script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net",
        "style-src 'self' 'unsafe-inline'",
        "font-src 'self'",
        "img-src 'self' data:",
        "connect-src 'self' https://*.execute-api.${var.region}.amazonaws.com https://cognito-idp.${var.region}.amazonaws.com",
        "frame-ancestors 'none'",
        "base-uri 'none'",
        "form-action 'none'",
      ])
    }
    strict_transport_security {
      override                   = true
      access_control_max_age_sec = 63072000
      include_subdomains         = true
      preload                    = true
    }
    content_type_options {
      override = true
    }
    frame_options {
      override     = true
      frame_option = "DENY"
    }
    referrer_policy {
      override        = true
      referrer_policy = "strict-origin-when-cross-origin"
    }
    xss_protection {
      override   = true
      protection = true
      mode_block = true
    }
  }
}

# --- CloudFront access log bucket ---------------------------------------------
# CloudFront standard logging delivers via ACL, so this bucket (unlike the site
# bucket) must keep ACLs enabled — hence the ownership_controls + acl pair. It is
# still fully private to the public.
resource "aws_s3_bucket" "logs" {
  bucket_prefix = "${local.name}-cf-logs-"
  force_destroy = false
}

resource "aws_s3_bucket_public_access_block" "logs" {
  bucket                  = aws_s3_bucket.logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_ownership_controls" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule {
    object_ownership = "BucketOwnerPreferred"
  }
}

data "aws_canonical_user_id" "current" {}

# Canonical user id of the CloudFront log delivery account (a fixed, AWS-published
# constant — not an account of ours).
locals {
  cloudfront_log_delivery_canonical_id = "c4c1ede66af53448b93c283ce9448c4ba468c9432aa01d700d3878632f77d2d0"
}

resource "aws_s3_bucket_acl" "logs" {
  depends_on = [
    aws_s3_bucket_ownership_controls.logs,
    aws_s3_bucket_public_access_block.logs,
  ]
  bucket = aws_s3_bucket.logs.id
  access_control_policy {
    grant {
      grantee {
        type = "CanonicalUser"
        id   = local.cloudfront_log_delivery_canonical_id
      }
      permission = "FULL_CONTROL"
    }
    grant {
      grantee {
        type = "CanonicalUser"
        id   = data.aws_canonical_user_id.current.id
      }
      permission = "FULL_CONTROL"
    }
    owner {
      id = data.aws_canonical_user_id.current.id
    }
  }
}

resource "aws_cloudfront_distribution" "site" {
  enabled             = true
  default_root_object = "console.html"
  comment             = "${local.name} site (console)"

  origin {
    domain_name              = aws_s3_bucket.site.bucket_regional_domain_name
    origin_id                = "s3-site"
    origin_access_control_id = aws_cloudfront_origin_access_control.site.id
  }

  default_cache_behavior {
    target_origin_id           = "s3-site"
    viewer_protocol_policy     = "redirect-to-https"
    allowed_methods            = ["GET", "HEAD"]
    cached_methods             = ["GET", "HEAD"]
    cache_policy_id            = "4135ea2d-6df8-44a3-9df3-4b5a84be39ad" # Managed-CachingDisabled
    response_headers_policy_id = aws_cloudfront_response_headers_policy.site.id
  }

  # Access logs: who requested what from the console distribution. Without this
  # there is no record of access to the admin surface at the edge.
  #
  # CloudFront validates, at UpdateDistribution time, that the log bucket has ACL
  # access enabled. Terraform sees no dependency between the distribution and the
  # bucket's ownership/ACL resources, so without the depends_on below it races and
  # fails with "does not enable ACL access" on a fresh deploy.
  logging_config {
    bucket          = aws_s3_bucket.logs.bucket_domain_name
    prefix          = "cloudfront/"
    include_cookies = false
  }

  depends_on = [
    aws_s3_bucket_ownership_controls.logs,
    aws_s3_bucket_acl.logs,
  ]

  restrictions {
    geo_restriction { restriction_type = "none" }
  }

  # Custom domain, when one is supplied. This is the ONLY way to control the TLS
  # floor: with the CloudFront default certificate AWS pins the minimum protocol
  # to TLSv1 and refuses any minimum_protocol_version — so a deployment that
  # needs TLS 1.2+ (most do) must bring a domain and an ACM certificate. The
  # certificate has to live in us-east-1, which is a CloudFront requirement.
  aliases = var.domain_name == null ? [] : [var.domain_name]

  viewer_certificate {
    cloudfront_default_certificate = var.acm_certificate_arn == null
    acm_certificate_arn            = var.acm_certificate_arn
    ssl_support_method             = var.acm_certificate_arn == null ? null : "sni-only"
    minimum_protocol_version       = var.acm_certificate_arn == null ? null : "TLSv1.2_2021"
  }

  price_class = "PriceClass_100"
}

resource "aws_s3_bucket_policy" "site" {
  bucket     = aws_s3_bucket.site.id
  depends_on = [aws_s3_bucket_public_access_block.site]
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowCloudFrontOAC"
      Effect    = "Allow"
      Principal = { Service = "cloudfront.amazonaws.com" }
      Action    = "s3:GetObject"
      Resource  = "${aws_s3_bucket.site.arn}/*"
      Condition = { StringEquals = { "AWS:SourceArn" = aws_cloudfront_distribution.site.arn } }
    }]
  })
}

# Inter font (variable, Latin subset, ~48 KB) served by the SAME bucket/CloudFront
# as the site. Self-hosted instead of Google Fonts: no user IP going to a third party
# (LGPD/GDPR), no extra DNS+TLS on the render path and no new CDN (project rule).
# The distribution uses Managed-CachingDisabled, so the edge does not cache — but the
# cache_control below makes the BROWSER cache, and each visitor downloads it once.
resource "aws_s3_object" "font_inter" {
  bucket        = aws_s3_bucket.site.id
  key           = "fonts/inter-var-latin.woff2"
  content_type  = "font/woff2"
  cache_control = "public, max-age=31536000, immutable"
  source        = "${local.site}/fonts/inter-var-latin.woff2"
  etag          = filemd5("${local.site}/fonts/inter-var-latin.woff2")
}

# Tailwind runtime, SELF-HOSTED for the same reason as the font: no third-party
# CDN on the admin surface. It cannot be pinned with Subresource Integrity from
# cdn.tailwindcss.com (that CDN sends no CORS header), so serving it from our own
# origin is the only way to control the bytes. Version is in the key, so the
# immutable cache is safe. See site/vendor/README.md.
resource "aws_s3_object" "vendor_tailwind" {
  bucket        = aws_s3_bucket.site.id
  key           = "vendor/tailwind-3.4.16.js"
  content_type  = "application/javascript"
  cache_control = "public, max-age=31536000, immutable"
  source        = "${local.site}/vendor/tailwind-3.4.16.js"
  etag          = filemd5("${local.site}/vendor/tailwind-3.4.16.js")
}

# env.js cache-buster: even with Cache-Control: no-cache, some browsers
# reuse an old copy of a <script src> without revalidating correctly. The tag
# in console.html loads env.js with ?v=<content hash>, so every time
# env.js changes the URL changes — the browser has no way to serve the wrong
# version from cache. env_content is computed once here and reused by both objects.
locals {
  env_content = templatefile("${local.site}/env.js.tftpl", {
    admin_api_endpoint = local.admin_api_endpoint
    gateway_endpoint   = local.gateway_endpoint
    usage_api_endpoint = local.usage_api_endpoint
    keyadmin_endpoint  = local.keyadmin_endpoint
    help_api_endpoint  = local.help_api_endpoint
    audit_api_endpoint = local.audit_api_endpoint
    region             = var.region
    cognito_client_id  = local.cognito_client_id
    cognito_domain     = local.cognito_domain
    social_providers   = local.social_providers
    platform_account   = data.aws_caller_identity.current.account_id
  })
  console_html = replace(
    file("${local.site}/app/console.html"),
    "<script src=\"env.js\"></script>",
    "<script src=\"env.js?v=${md5(local.env_content)}\"></script>",
  )
}

resource "aws_s3_object" "console" {
  bucket        = aws_s3_bucket.site.id
  key           = "console.html"
  content_type  = "text/html"
  cache_control = "no-cache"
  content       = local.console_html
  etag          = md5(local.console_html)
}

resource "aws_s3_object" "env" {
  bucket        = aws_s3_bucket.site.id
  key           = "env.js"
  content_type  = "application/javascript"
  cache_control = "no-cache"
  content       = local.env_content
  etag          = md5(local.env_content)
}

# BYO Bedrock onboarding no longer uses a CloudFormation template generated by the
# platform: the client creates the AIPlatGatewayAccess* role manually in their own
# account (console.html shows the parameters — platform account + External ID —
# and the required permissions). No file to host, no download URL.

output "site_bucket" { value = aws_s3_bucket.site.id }
output "console_url" { value = "https://${aws_cloudfront_distribution.site.domain_name}/console.html" }

# --- Environment Contract (SSM): frontend publishes its URL ---
resource "aws_ssm_parameter" "console_url" {
  name  = "${local.ssm_root}/frontend/console_url"
  type  = "String"
  value = "https://${aws_cloudfront_distribution.site.domain_name}/console.html"
}
