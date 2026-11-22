import { Button, Flex, Result, Spin } from 'antd'
import { useEffect, useState } from 'react'
import { Navigate, Outlet, useLocation, type Location } from 'react-router'
import { getMe } from '../api/auth'
import { ApiError, Code, errorMessage } from '../api/client'
import { useAuthStore } from '../stores/auth'

// 守卫跳转登录页时，把原来要去的地址放进 location.state，登录后跳回去
interface FromState {
  from?: Location
}

// RequireAuth 需要登录的页面。类比 Spring Security 的拦截器：
// 没有令牌 → 跳转登录页；有令牌但还没有用户信息（刷新页面后）→ 调 /me 获取，顺便验证令牌。
export function RequireAuth() {
  const token = useAuthStore((s) => s.token)
  const user = useAuthStore((s) => s.user)
  const setUser = useAuthStore((s) => s.setUser)
  const location = useLocation()
  const [error, setError] = useState<string | null>(null)
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    if (!token || user) return
    // 令牌在请求途中变了（退出、其他标签页切换账号），丢弃旧请求的结果
    let cancelled = false
    getMe()
      .then((u) => {
        if (!cancelled) setUser(u)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        // 10003 时 api 层已清除令牌，下一次渲染会走到下面的 Navigate，这里不用处理
        if (err instanceof ApiError && err.code === Code.Unauthorized) return
        setError(errorMessage(err))
      })
    return () => {
      cancelled = true
    }
  }, [token, user, setUser, attempt])

  if (!token) {
    return <Navigate to="/login" replace state={{ from: location } satisfies FromState} />
  }
  if (error) {
    return (
      <Result
        status="warning"
        title="获取用户信息失败"
        subTitle={error}
        extra={
          <Button
            type="primary"
            onClick={() => {
              setError(null)
              setAttempt((n) => n + 1)
            }}
          >
            重试
          </Button>
        }
      />
    )
  }
  if (!user) {
    return (
      <Flex align="center" justify="center" style={{ minHeight: '100vh' }}>
        <Spin size="large" />
      </Flex>
    )
  }
  return <Outlet />
}

// GuestOnly 只给未登录用户看的页面（登录、注册）。已登录则跳回原来要去的页面或首页。
export function GuestOnly() {
  const token = useAuthStore((s) => s.token)
  const location = useLocation()

  if (token) {
    const from = (location.state as FromState | null)?.from?.pathname ?? '/'
    return <Navigate to={from} replace />
  }
  return <Outlet />
}
