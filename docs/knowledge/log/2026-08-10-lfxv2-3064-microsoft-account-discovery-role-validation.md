# 2026-08-10 — Microsoft account discovery: role-based write permission validation

**Update** — `internal/platform/microsoft/accounts.go` adds role-based validation to `Usable()` and caps the number of customers that can be discovered.

## Problem: Viewer role is read-only but Usable() returned true

Microsoft's `User/Query` response includes a `RoleId` field for each `CustomerRole` that identifies the permission level of the credentials for that customer. Role 100 is Viewer, which has read-only permission and cannot create campaigns.

The prior implementation:
1. Extracted only `CustomerId` from `CustomerRole`, discarding `RoleId`
2. Used all discovered customers to enumerate accounts
3. Returned all accounts with `Usable() == true` if `Status == "Active" && PauseReason == 0`

This meant that an account discovered through a Viewer-role customer would be advertised as usable, but campaign creation would fail when attempted.

## Solution: propagate role through discovery chain

The fix extracts and propagates `RoleId` through the discovery chain:

1. **customerRole struct** now captures both `CustomerId` and `RoleId`
2. **discoveredCustomer struct** added to carry both the customer id and its role id
3. **discoveryCustomerIDs** now returns `[]discoveredCustomer` instead of `[]string`, including role information for both pre-configured customers (role 0 = unknown) and discovered customers
4. **ListAdAccounts** passes the role to `accountsInfoForCustomer`
5. **accountsInfoForCustomer** now accepts `roleID` and populates `AdAccount.RoleID`
6. **Usable()** now checks that `RoleID != 100` (Viewer), returning false for read-only accounts

Pre-configured customers (those supplied via `AccountConfig.CustomerID`) use role 0 to indicate unknown role, allowing them through the `Usable()` check since we cannot validate them.

## Problem: unbounded CustomerRoles array

The code enumerated all discovered customers without any upper bound. A user with a very large customer hierarchy could trigger:
- Unbounded number of `AccountsInfo/Query` requests
- Unbounded time consumed (each request may retry)
- Unbounded request quota consumption

## Solution: cap customer count at 1000

The fix adds a cap of 1000 customers. If `User/Query` returns more than 1000 `CustomerRoles`, discovery fails:

```go
const maxCustomers = 1000
if len(*resp.CustomerRoles) > maxCustomers {
    return nil, fmt.Errorf(...)
}
```

This preserves the fail-not-truncate contract: a partial union is indistinguishable from a complete one at the boundary, so truncating silently would report a protocol mismatch as a permissions problem. The cap is high enough for legitimate use (few users manage 1000+ customers) while preventing amplification attacks.
