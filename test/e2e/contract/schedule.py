#!/usr/bin/env python3
"""What a contract should bill, worked out from its Offer's snapshot.

Reads the Offer (kubectl get offer -o json) on stdin and prints JSON:

  schedule.py items STARTS_AT
      The subscription items the Offer's fees should become, one per fixed
      fee: its rate card name, billing cadence, payment term, amount (the
      fee's total) and, for a fee with periods, when it ends.

  schedule.py invoices STARTS_AT [MONTHS [AMENDED_OFFER_FILE AT_MONTH]]
      The invoices the fixed fees produce from the contract's start through
      MONTHS months (12 by default), one per billing date that charges
      anything, each with its lines: name, total and service period. With
      an amended Offer (a file of kubectl get offer -o json) taking effect
      AT_MONTH months in, the fees it adds start then; the fees both Offers
      have carry on as they were, never charged again.

The rules are billing's, written out here independently of the provider so
the suites check the provider against them rather than against itself:

  - every date falls on the contract's billing day: startsAt's day of the
    month, or the month's last day when it's shorter, in UTC;
  - a one-time fee is charged on the start date;
  - a recurring fee is charged every 1, 3 or 12 months (its interval): in
    advance at the start of each period, or in arrears at its end;
  - a fee with periods stops after that many: billed in advance, its last
    charge is at the start of its last period; in arrears, at its end;
  - a fee's total is its amount times its quantity (1 when unset), and its
    line is named as the provider's feeName names it.

  schedule.py amendment STARTS_AT AFTER_MONTH
      When an amendment made AFTER_MONTH months in takes effect: the next
      billing date that ends a period of every fee billed in advance, so no
      fee is charged twice for the same months. With a quarterly fee billed
      in advance, the next quarter's start; with an annual one, the
      contract's next anniversary; otherwise the next billing date.

Usage is left out: what it bills depends on the usage published.
"""
import json
import sys
from datetime import datetime, timezone
from decimal import Decimal

INTERVAL_MONTHS = {"": 1, "monthly": 1, "quarterly": 3, "annual": 12}
CADENCE = {1: "P1M", 3: "P3M", 12: "P1Y"}


def parse_time(value):
    return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)


def fmt_time(value):
    return value.strftime("%Y-%m-%dT%H:%M:%SZ")


def billing_date(start, months):
    """start moved on by months, on start's day or the month's last day."""
    month0 = start.month - 1 + months
    year, month = start.year + month0 // 12, month0 % 12 + 1
    next0 = month0 + 1
    last = (datetime(start.year + next0 // 12, next0 % 12 + 1, 1, tzinfo=timezone.utc)
            - datetime(year, month, 1, tzinfo=timezone.utc)).days
    return start.replace(year=year, month=month, day=min(start.day, last))


def fees(offer):
    """The Offer's fixed fees, in snapshot order."""
    out = []
    for sp in offer["spec"].get("servicePricings", []):
        spec = sp["spec"]
        if spec["chargeType"] not in ("OneTime", "Recurring"):
            continue
        recurring = spec["chargeType"] == "Recurring"
        quantity = sp.get("quantity") or 1
        name = spec.get("displayName") or sp["name"]
        if spec.get("pricingUnit"):
            name = f"{name}: {quantity} {spec['pricingUnit']} × ${spec['amount']}"
        elif quantity > 1:
            name = f"{name} × {quantity}"
        out.append({
            "pricing": sp["name"],
            "name": name,
            "recurring": recurring,
            "every": INTERVAL_MONTHS[spec.get("interval", "")] if recurring else None,
            "inArrears": recurring and sp.get("billingTiming") == "InArrears",
            "periods": sp.get("periods") if recurring else None,
            "total": Decimal(spec["amount"]) * quantity,
        })
    return out


def items(offer, start):
    out = []
    for fee in fees(offer):
        item = {
            "name": fee["name"],
            "cadence": CADENCE[fee["every"]] if fee["recurring"] else None,
            "paymentTerm": "in_arrears" if fee["inArrears"] else "in_advance",
            "amount": str(fee["total"]),
        }
        if fee["periods"]:
            item["endsAt"] = fmt_time(billing_date(start, fee["periods"] * fee["every"]))
        out.append(item)
    return out


def invoices(offer, start, months, amended=None, at=0):
    contract = fees(offer)
    known = {fee["pricing"] for fee in contract}
    if amended is not None:
        contract += [dict(fee, since=at) for fee in fees(amended) if fee["pricing"] not in known]
    out = []
    for month in range(months + 1):
        lines = []
        for fee in contract:
            since = fee.get("since", 0)
            k = month - since
            if k < 0:
                continue
            if not fee["recurring"]:
                if k == 0:
                    day = fmt_time(billing_date(start, since))
                    lines.append({"name": fee["name"], "total": str(fee["total"]), "from": day, "to": day})
                continue
            every = fee["every"]
            if k % every:
                continue
            last = fee["periods"] * every if fee["periods"] else None
            if fee["inArrears"]:
                if k == 0 or (last is not None and k > last):
                    continue
                period = (k - every, k)
            else:
                if last is not None and k >= last:
                    continue
                period = (k, k + every)
            lines.append({"name": fee["name"], "total": str(fee["total"]),
                          "from": fmt_time(billing_date(start, since + period[0])),
                          "to": fmt_time(billing_date(start, since + period[1]))})
        if lines:
            total = sum(Decimal(line["total"]) for line in lines)
            out.append({"month": month, "date": fmt_time(billing_date(start, month)),
                        "total": str(total), "lines": lines})
    return out


def amendment(offer, start, after):
    every = max([fee["every"] for fee in fees(offer) if fee["recurring"] and not fee["inArrears"]] or [1])
    month = (after // every + 1) * every
    return {"month": month, "date": fmt_time(billing_date(start, month))}


def main():
    command, start = sys.argv[1], parse_time(sys.argv[2])
    offer = json.load(sys.stdin)
    if command == "items":
        result = items(offer, start)
    elif command == "invoices":
        months = int(sys.argv[3]) if len(sys.argv) > 3 else 12
        amended, at = None, 0
        if len(sys.argv) > 5:
            with open(sys.argv[4]) as f:
                amended = json.load(f)
            at = int(sys.argv[5])
        result = invoices(offer, start, months, amended, at)
    elif command == "amendment":
        result = amendment(offer, start, int(sys.argv[3]))
    else:
        sys.exit(f"unknown command {command!r}")
    json.dump(result, sys.stdout)
    print()


if __name__ == "__main__":
    main()
