## ADDED Requirements

### Requirement: User login
系统 SHALL 提供用户登录接口，接受 username 和 password，验证通过后签发 JWT token。

#### Scenario: Successful login
- **WHEN** 用户发送 POST /api/v1/auth/login，body 包含有效的 username 和 password
- **THEN** 系统返回 HTTP 200，body 包含 access_token 和 expire 信息

#### Scenario: Invalid credentials
- **WHEN** 用户发送 POST /api/v1/auth/login，password 不匹配
- **THEN** 系统返回 HTTP 401，body 包含错误信息 "invalid credentials"

#### Scenario: User not found
- **WHEN** 用户发送 POST /api/v1/auth/login，username 不存在
- **THEN** 系统返回 HTTP 401，body 包含错误信息 "user not found"

### Requirement: JWT token structure
签发的 JWT token SHALL 包含 user_id (UUID) claim，使用 HS256 算法签名，过期时间由配置文件 jwt.expire 控制。

#### Scenario: Token contains user_id
- **WHEN** 系统签发 JWT token
- **THEN** token 的 payload 中包含 "user_id" 字段，值为用户的 UUID

#### Scenario: Token respects configured expiry
- **WHEN** 配置文件 jwt.expire 设置为 "7d"
- **THEN** 签发的 token 过期时间为 7 天后

### Requirement: Auth middleware
系统 SHALL 提供 JWT 鉴权中间件，验证所有需要登录的接口的 Authorization header。

#### Scenario: Valid token in header
- **WHEN** 请求携带 Authorization: Bearer <valid_token>
- **THEN** 中间件解析 token，将 user_id 注入 gin.Context，请求继续处理

#### Scenario: Missing token
- **WHEN** 请求不携带 Authorization header
- **THEN** 中间件返回 HTTP 401，body 包含 "missing token"

#### Scenario: Expired token
- **WHEN** 请求携带过期的 JWT token
- **THEN** 中间件返回 HTTP 401，body 包含 "token expired"

#### Scenario: Invalid token
- **WHEN** 请求携带格式错误或签名不匹配的 token
- **THEN** 中间件返回 HTTP 401，body 包含 "invalid token"

### Requirement: Password hashing
用户密码 SHALL 使用 bcrypt 加密存储，禁止明文存储。

#### Scenario: Password stored as hash
- **WHEN** 系统验证用户登录密码
- **THEN** 系统使用 bcrypt.CompareHashAndPassword 比对密码哈希
