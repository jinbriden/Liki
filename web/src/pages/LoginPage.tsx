import { Alert, Button, Form, Input } from 'antd'
import { useState } from 'react'
import { Link, useLocation } from 'react-router'
import { login } from '../api/auth'
import { Code, errorMessage, isApiError } from '../api/client'
import { AuthCard } from '../components/AuthCard'
import { useAuthStore } from '../stores/auth'
import { isValidUserId, MSG_INVALID_USER_ID } from '../utils/validators'

interface LoginForm {
  userId: string
  password: string
}

// 注册成功后点"去登录"，注册页会带着用户 ID 跳过来，预先填好
interface LoginPageState {
  userId?: number
}

export default function LoginPage() {
  const setAuth = useAuthStore((s) => s.setAuth)
  const location = useLocation()
  const prefillId = (location.state as LoginPageState | null)?.userId
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<{ msg: string; type: 'error' | 'warning' } | null>(null)

  async function onFinish(values: LoginForm) {
    setSubmitting(true)
    setError(null)
    try {
      const res = await login(Number(values.userId), values.password)
      // 写入令牌后，GuestOnly 守卫会自动跳转到首页（或登录前想去的页面）
      setAuth(res.token, res.user)
    } catch (err) {
      // 20001（ID 或密码错误）留在本页提示，不跳转。
      // 20002 / 20003（密码正确但账号待审核 / 未通过，FR-22）不是输错了，用警告样式区分
      const reviewing = isApiError(err, Code.AccountPending, Code.AccountRejected)
      setError({ msg: errorMessage(err), type: reviewing ? 'warning' : 'error' })
      setSubmitting(false)
    }
  }

  return (
    <AuthCard title="登录">
      {error && <Alert type={error.type} title={error.msg} showIcon style={{ marginBottom: 16 }} />}
      <Form<LoginForm>
        layout="vertical"
        requiredMark={false}
        initialValues={{ userId: prefillId ? String(prefillId) : '' }}
        onFinish={onFinish}
        onValuesChange={() => setError(null)}
      >
        <Form.Item
          name="userId"
          label="用户 ID"
          rules={[
            { required: true, message: '请输入用户 ID' },
            {
              validator: (_, v: string) =>
                !v || isValidUserId(v) ? Promise.resolve() : Promise.reject(new Error(MSG_INVALID_USER_ID)),
            },
          ]}
        >
          <Input inputMode="numeric" maxLength={10} autoComplete="username" placeholder="注册时分配的 10 位数字" />
        </Form.Item>
        {/* 登录时只检查非空，不检查复杂度：以后调整注册规则时，老用户仍要能登录 */}
        <Form.Item name="password" label="密码" rules={[{ required: true, message: '请输入密码' }]}>
          <Input.Password autoComplete="current-password" />
        </Form.Item>
        <Form.Item style={{ marginBottom: 12 }}>
          <Button type="primary" htmlType="submit" block loading={submitting}>
            登录
          </Button>
        </Form.Item>
      </Form>
      <div style={{ textAlign: 'center' }}>
        有邀请码？<Link to="/register">注册</Link>
      </div>
    </AuthCard>
  )
}
