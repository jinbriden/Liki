import { Alert, Button, Form, Input, Spin, Typography } from 'antd'
import { useEffect, useState } from 'react'
import { Link, useLocation, useNavigate } from 'react-router'
import { register } from '../api/auth'
import { Code, errorMessage, isApiError } from '../api/client'
import { AuthCard } from '../components/AuthCard'
import { attachRegisterPage, useInviteReservation } from '../stores/inviteReservation'
import { formatTime } from '../utils/time'
import {
  isValidInviteCode,
  isValidPassword,
  isValidUsername,
  MSG_INVALID_INVITE_CODE,
  MSG_INVALID_PASSWORD,
  MSG_INVALID_USERNAME,
  normalizeInviteCode,
} from '../utils/validators'

// inviteFromHash 读取邀请链接 /register#invite=XXXXXX 里的邀请码（FR-23）
function inviteFromHash(hash: string): string | null {
  const code = new URLSearchParams(hash.slice(1)).get('invite')
  return code === null ? null : normalizeInviteCode(code)
}

// RegisterPage 注册分两步（FR-23）：先验证邀请码（通过即冻结），通过后才显示注册表单。
// 冻结的续期、离开时解冻都在 stores/inviteReservation.ts
export default function RegisterPage() {
  const navigate = useNavigate()
  const location = useLocation()
  const phase = useInviteReservation((s) => s.phase)
  const verify = useInviteReservation((s) => s.verify)
  const resume = useInviteReservation((s) => s.resume)
  // 注册成功后的用户 ID：账号要等管理员审核（FR-22），不自动登录，改为显示结果页
  const [registeredId, setRegisteredId] = useState<number | null>(null)

  // 挂载时开始续期，离开注册页时解冻
  useEffect(() => attachRegisterPage(), [])

  // 通过邀请链接打开：自动验证，然后把邀请码从地址栏去掉（不留在浏览历史、书签和截图里）。
  // 否则如果刷新前已经验证过（sessionStorage 里有令牌），续期恢复冻结。
  // 去掉 hash 会让 location 变化、这里再执行一次：那时已经在验证中，resume 什么也不做（只在 idle 时生效）
  useEffect(() => {
    const code = inviteFromHash(location.hash)
    if (code !== null) {
      navigate({ pathname: location.pathname, search: location.search }, { replace: true })
      if (isValidInviteCode(code)) void verify(code)
    } else {
      void resume()
    }
  }, [location, navigate, verify, resume])

  if (registeredId !== null) {
    return (
      <AuthCard title="注册申请已提交">
        <p style={{ marginTop: 0 }}>你的用户 ID 是（登录时需要用到，请复制保存好）：</p>
        <Typography.Title level={3} copyable style={{ marginTop: 0 }}>
          {String(registeredId)}
        </Typography.Title>
        <p style={{ color: 'var(--liki-text-2)' }}>账号需要管理员审核，审核通过后用这个 ID 和密码登录。</p>
        <Button type="primary" block onClick={() => navigate('/login', { state: { userId: registeredId } })}>
          去登录
        </Button>
      </AuthCard>
    )
  }

  if (phase === 'held') {
    return <RegisterForm onRegistered={setRegisteredId} />
  }
  if (phase === 'resuming') {
    return (
      <AuthCard title="注册新账号">
        <div style={{ textAlign: 'center', padding: 24 }}>
          <Spin />
        </div>
      </AuthCard>
    )
  }
  return <InviteGate />
}

interface GateForm {
  inviteCode: string
}

// InviteGate 第 1 步：输入邀请码。无效的码进不了注册表单
function InviteGate() {
  const verify = useInviteReservation((s) => s.verify)
  const verifying = useInviteReservation((s) => s.phase === 'verifying')
  const lastCode = useInviteReservation((s) => s.lastCode)
  const error = useInviteReservation((s) => s.error)
  const clearError = useInviteReservation((s) => s.clearError)

  return (
    <AuthCard title="注册新账号">
      <Typography.Paragraph type="secondary" style={{ marginTop: -8 }}>
        目前处于内测阶段，需要邀请码才能注册。
      </Typography.Paragraph>
      {error && <Alert type="error" title={error} showIcon style={{ marginBottom: 16 }} />}
      <Form<GateForm>
        layout="vertical"
        requiredMark={false}
        initialValues={{ inviteCode: lastCode }}
        onFinish={(v) => void verify(normalizeInviteCode(v.inviteCode))}
        onValuesChange={clearError}
      >
        <Form.Item
          name="inviteCode"
          label="邀请码"
          extra="6 位数字或英文字母，由邀请人提供"
          // 输入时直接转成大写，和最终提交的值一致
          normalize={(v: string) => v.toUpperCase()}
          rules={[
            { required: true, message: '请输入邀请码' },
            {
              validator: (_, v: string) =>
                !v || isValidInviteCode(v) ? Promise.resolve() : Promise.reject(new Error(MSG_INVALID_INVITE_CODE)),
            },
          ]}
        >
          <Input
            autoFocus
            maxLength={6}
            autoComplete="off"
            spellCheck={false}
            style={{ fontFamily: 'var(--liki-mono)', letterSpacing: 2 }}
          />
        </Form.Item>
        <Form.Item style={{ marginBottom: 12 }}>
          <Button type="primary" htmlType="submit" block loading={verifying}>
            验证邀请码
          </Button>
        </Form.Item>
      </Form>
      <div style={{ textAlign: 'center' }}>
        已有账号？<Link to="/login">登录</Link>
      </div>
    </AuthCard>
  )
}

interface RegisterFormValues {
  username: string
  password: string
  confirm: string
}

// RegisterForm 第 2 步：邀请码已验证（冻结中），填写并提交注册申请
function RegisterForm({ onRegistered }: { onRegistered: (id: number) => void }) {
  const held = useInviteReservation((s) => s.held)!
  const release = useInviteReservation((s) => s.release)
  const markUsed = useInviteReservation((s) => s.markUsed)
  const lose = useInviteReservation((s) => s.lose)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function onFinish(values: RegisterFormValues) {
    setSubmitting(true)
    setError(null)
    try {
      const res = await register(values.username, values.password, held.code, held.token)
      markUsed() // 提交成功，邀请码已失效
      onRegistered(res.id)
    } catch (err) {
      if (isApiError(err, Code.ReservationLost)) {
        lose(errorMessage(err)) // 冻结到期、码被作废或过期：回到第 1 步
        return
      }
      setError(errorMessage(err))
      setSubmitting(false)
    }
  }

  return (
    <AuthCard title="注册新账号">
      <div className="invite-verified">
        <span>
          邀请码 <span className="invite-code">{held.code}</span> 已验证
          <span className="invite-verified-expiry"> · {formatTime(held.expiresAt)} 前有效</span>
        </span>
        <Button type="link" size="small" onClick={release} style={{ padding: 0 }}>
          更换
        </Button>
      </div>
      {error && <Alert type="error" title={error} showIcon style={{ marginBottom: 16 }} />}
      <Form<RegisterFormValues>
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
          <Input autoFocus maxLength={32} autoComplete="username" />
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
            提交注册申请
          </Button>
        </Form.Item>
      </Form>
      <div style={{ textAlign: 'center', color: 'var(--liki-text-3)', fontSize: 12 }}>
        邀请码只能提交一次注册申请，提交后立即失效
      </div>
    </AuthCard>
  )
}
