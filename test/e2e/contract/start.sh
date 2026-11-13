#!/bin/bash
# Prints midnight UTC today: when the suite's contract starts. Billing only
# admits contracts starting today or later, and the staff portal starts one
# at midnight UTC on the day picked.
set -euo pipefail
date -u +%Y-%m-%dT00:00:00Z
