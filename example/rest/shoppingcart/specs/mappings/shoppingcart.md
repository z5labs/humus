# Data Mappings: Shopping Cart API ↔ Database

All operations map camelCase API fields to snake_case database columns.

## API → Database Column Mappings

| API Field   | Database Table | Column       |
|-------------|----------------|--------------|
| `cartId`    | `carts`        | `id`         |
| `cartId`    | `cart_items`   | `cart_id`    |
| `itemId`    | `cart_items`   | `id`         |
| `productId` | `cart_items`   | `product_id` |
| `quantity`  | `cart_items`   | `quantity`   |
| `unitPrice` | `cart_items`   | `unit_price` |
| `createdAt` | both           | `created_at` |
| `updatedAt` | both           | `updated_at` |

## Database → API Field Mappings

| Database Table | Column       | API Field   |
|----------------|--------------|-------------|
| `carts`        | `id`         | `cartId`    |
| `carts`        | `created_at` | `createdAt` |
| `carts`        | `updated_at` | `updatedAt` |
| `cart_items`   | `id`         | `itemId`    |
| `cart_items`   | `cart_id`    | `cartId`    |
| `cart_items`   | `product_id` | `productId` |
| `cart_items`   | `quantity`   | `quantity`  |
| `cart_items`   | `unit_price` | `unitPrice` |
| `cart_items`   | `created_at` | `createdAt` |
| `cart_items`   | `updated_at` | `updatedAt` |

## Per-Operation Notes

| Operation       | Request Mapping          | Response Mapping                         |
|-----------------|--------------------------|------------------------------------------|
| `getCart`       | `cartId` → `carts.id`   | `carts.*` + `cart_items.*` → `Cart`      |
| `createCart`    | none                     | `carts.*` → `Cart` (items: [])           |
| `addCartItem`   | `cartId` → `cart_items.cart_id`, `productId` → `product_id`, `unitPrice` → `unit_price` | `cart_items.*` → `CartItem` |
| `updateCartItem`| `cartId` → `cart_id`, `itemId` → `id`, `quantity` → `quantity` | `cart_items.*` → `CartItem` |
| `removeCartItem`| `cartId` → `cart_id`, `itemId` → `id` | 204 No Content |
| `deleteCart`    | `cartId` → `carts.id`   | 204 No Content                           |
