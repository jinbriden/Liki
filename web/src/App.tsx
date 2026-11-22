import { createBrowserRouter, Navigate, RouterProvider } from 'react-router'
import MainLayout from './layouts/MainLayout'
import ChatPage from './pages/ChatPage'
import FriendDetail from './pages/friends/FriendDetail'
import FriendsPage, { FriendsEmpty } from './pages/friends/FriendsPage'
import RequestsPanel from './pages/friends/RequestsPanel'
import LoginPage from './pages/LoginPage'
import RegisterPage from './pages/RegisterPage'
import { GuestOnly, RequireAuth } from './routes/guards'

const router = createBrowserRouter([
  {
    element: <GuestOnly />,
    children: [
      { path: '/login', element: <LoginPage /> },
      { path: '/register', element: <RegisterPage /> },
    ],
  },
  {
    // RequireAuth（登录校验）→ MainLayout（导航栏）→ 具体页面，逐层嵌套
    element: <RequireAuth />,
    children: [
      {
        element: <MainLayout />,
        children: [
          { path: '/', element: <Navigate to="/friends" replace /> },
          {
            path: '/friends',
            element: <FriendsPage />,
            children: [
              { index: true, element: <FriendsEmpty /> },
              { path: 'requests', element: <RequestsPanel /> }, // 静态路径优先于 :friendId 匹配
              { path: ':friendId', element: <FriendDetail /> },
            ],
          },
          { path: '/chat', element: <ChatPage /> },
          { path: '/chat/:peerId', element: <ChatPage /> },
        ],
      },
    ],
  },
  { path: '*', element: <Navigate to="/" replace /> },
])

export default function App() {
  return <RouterProvider router={router} />
}
