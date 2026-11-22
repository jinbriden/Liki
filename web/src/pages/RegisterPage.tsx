import { Alert, App, Button, Form, Input, Typography } from 'antd'
import { useState } from 'react'
import { Link, useNavigate } from 'react-router'
import { login, register } from '../api/auth'
import { errorMessage } from '../api/client'
import { AuthCard } from '../components/AuthCard'
import { useAuthStore } from '../stores/auth'
import { isValidPassword, isValidUsername, MSG_INVALID_PASSWORD, MSG_INVALID_USERNAME } from '../utils/validators'

interface RegisterForm {
  username: string
  password: string
  confirm: string
}

export default function RegisterPage() {
  const setAuth = useAuthStore((s) => s.setAuth)
  const setNewUserId = useAuthStore((s) => s.setNewUserId)
  const navigate = useNavigate()
  const { modal } = App.useApp()
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function onFinish(values: RegisterForm) {
    setSubmitting(true)
    setError(null)

    let userId: number
    try {
      const user = await register(values.username, values.password)
      userId = user.id
    } catch (err) {
      setError(errorMessage(err))
      setSubmitting(false)
      return
    }

    // 注册成功后用同一个密码自动登录，省去用户再输一次
    try {
      const res = await login(userId, values.password)
      // 先记下新用户 ID，首页会弹窗提醒用户记住它；再写入令牌，GuestOnly 守卫随即跳转首页
      setNewUserId(userId)
      setAuth(res.token, res.user)
    } catch {
      // 账号已经建好，只是自动登录没成功（如网络抖动）：告知 ID，带着 ID 去登录页手动登录
      setSubmitting(false)
      modal.warning({
        title: '注册成功，但自动登录失败',
        content: (
          <>
            <p>你的用户 ID 是（登录时需要用到，请牢记）：</p>
            <Typography.Text copyable strong style={{ fontSize: 20 }}>
              {String(userId)}
            </Typography.Text>
          </>
        ),
        okText: '去登录',
        onOk: () => navigate('/login', { state: { userId } }),
      })
    }
  }

  return (
    <AuthCard title="注册新账号">
      {error && <Alert type="error" title={error} showIcon style={{ marginBottom: 16 }} />}
      <Form<RegisterForm>
        layout="vertical"
        requiredMark={false}
        onFinish={onFinish}
        onValuesChange={() => setError(null)}
      >
        <Form.Item
          name="username"
          label="用户名"
          extra="3-32 位英文字母或数字，可以与他人重名"
          rules={[
            { required: true, message: '请输入用户名' },
            {
              validator: (_, v: string) =>
                !v || isValidUsername(v) ? Promise.resolve() : Promise.reject(new Error(MSG_INVALID_USERNAME)),
            },
          ]}
        >
          <Input maxLength={32} autoComplete="username" />
        </Form.Item>
        <Form.Item
          name="password"
          label="密码"
          extra="8-24 位，至少包含一个大写字母、一个小写字母和一个数字"
          rules={[
            { required: true, message: '请输入密码' },
            {
              validator: (_, v: string) =>
                !v || isValidPassword(v) ? Promise.resolve() : Promise.reject(new Error(MSG_INVALID_PASSWORD)),
            },
          ]}
        >
          <Input.Password maxLength={24} autoComplete="new-password" />
        </Form.Item>
        <Form.Item
          name="confirm"
          label="确认密码"
          dependencies={['password']} // 密码改了，确认密码重新校验
          rules={[
            { required: true, message: '请再次输入密码' },
            ({ getFieldValue }) => ({
              validator: (_, v: string) =>
                !v || v === getFieldValue('password')
                  ? Promise.resolve()
                  : Promise.reject(new Error('两次输入的密码不一致')),
            }),
          ]}
        >
          <Input.Password maxLength={24} autoComplete="new-password" />
        </Form.Item>
        <Form.Item style={{ marginBottom: 12 }}>
          <Button type="primary" htmlType="submit" block loading={submitting}>
            注册
          </Button>
        </Form.Item>
      </Form>
      <div style={{ textAlign: 'center' }}>
        已有账号？<Link to="/login">登录</Link>
      </div>
    </AuthCard>
  )
}
